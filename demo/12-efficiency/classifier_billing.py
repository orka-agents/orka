"""Retain exact billing receipts for unmetered Jev failures.

A 503 alone does not establish zero usage or cost. A safe generation ID in
Vekil's response log ties the failed call to Vercel's read-only billing API.
Gateway token counters remain incomplete even when billing proves no charge.
"""

from decimal import Decimal, InvalidOperation
import json
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request

import yaml


ENDPOINT = "https://ai-gateway.vercel.sh/v1/generation"
GENERATION = re.compile(r"gen_[A-Za-z0-9]{26}\Z")
MONEY_FIELDS = ("total_cost", "market_cost", "surcharge_cost", "gateway_cost", "upstream_inference_cost")
TOKEN_FIELDS = ("tokens_prompt", "tokens_completion", "native_tokens_prompt", "native_tokens_completion",
                "native_tokens_reasoning", "native_tokens_cached", "native_tokens_cache_creation",
                "billable_web_search_calls")


class BillingError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise BillingError(message)


def generation_url(identity):
    require(isinstance(identity, str) and GENERATION.fullmatch(identity), "Missing or invalid Jev generation ID")
    return ENDPOINT + "?" + urllib.parse.urlencode({"id": identity})


def verify_zero_charge(failure, receipt):
    """Require an exact, explicit zero-market-cost receipt, not a promotion debit."""
    identity = failure.get("generationID")
    url = generation_url(identity)
    require(type(failure.get("statusCode")) is int and 500 <= failure["statusCode"] <= 599
            and failure.get("reportedUsage") is False, "This is not an unmetered upstream failure")
    require(isinstance(receipt, dict) and receipt.get("url") == url and receipt.get("id") == identity
            and receipt.get("httpStatus") == 200, "Billing lookup does not identify the failed request")
    data = receipt.get("response", {}).get("data", {})
    require(data.get("id") == identity and data.get("model") == "typesafe-ai/jev"
            and data.get("provider_name") == "typesafe-ai" and data.get("is_byok") is False
            and data.get("finish_reason") == "error", "Billing record is not the matching failed Jev request")
    for field in MONEY_FIELDS:
        value = data.get(field)
        require(type(value) in (int, float, str), "Missing billing cost: " + field)
        try:
            amount = Decimal(str(value))
        except InvalidOperation as error:
            raise BillingError("Invalid billing cost: " + field) from error
        require(amount.is_finite() and amount == 0, "Failed classification has a nonzero or unknown cost")
    for field in TOKEN_FIELDS:
        require(type(data.get(field)) is int and data[field] == 0, "Failed classification has nonzero or unknown billed usage")
    return {"generationID": identity, "marketCostUsd": "0", "gatewayCostUsd": "0"}


def verify_receipts(failures, receipts):
    require(isinstance(failures, list) and isinstance(receipts, list), "Missing classifier billing evidence")
    identities = [row.get("generationID") for row in failures]
    require(len(set(identities)) == len(identities), "Duplicate failed classifier generation")
    saved_ids = [row.get("id") for row in receipts]
    require(len(set(saved_ids)) == len(saved_ids) and set(saved_ids) == set(identities),
            "Missing, duplicate, or unrelated classifier billing receipt")
    saved = {row["id"]: row for row in receipts}
    return [verify_zero_charge(row, saved[row["generationID"]]) for row in failures]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Never forward this credential to another host or an unexpected path.
        return None


def fetch_receipts(failures, source, directory):
    """GET billing data only. No inference call or credential is written to disk."""
    if not failures:
        return []
    config = yaml.safe_load(Path(source).read_text())
    providers = [row for row in config["providers"]
                 if row.get("type") == "typesafe-compatible"
                 and urllib.parse.urlsplit(row.get("base_url", "")).hostname == "ai-gateway.vercel.sh"]
    require(len(providers) == 1 and isinstance(providers[0].get("api_key"), str)
            and providers[0]["api_key"], "The authorized Vercel credential is unavailable")
    key = providers[0]["api_key"]
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    opener = urllib.request.build_opener(NoRedirect())
    receipts = []
    for failure in failures:
        identity = failure["generationID"]
        url = generation_url(identity)
        path = directory / (identity + ".json")
        if path.exists():
            receipt = json.loads(path.read_text())
        else:
            request = urllib.request.Request(url, headers={"Authorization": "Bearer " + key})
            # Billing indexing can lag the completed request. Retrying this GET
            # does not repeat the classifier or hide an inference attempt.
            for attempt in range(6):
                try:
                    with opener.open(request, timeout=20) as response:
                        receipt = {"url": url, "id": identity, "httpStatus": response.status,
                                   "response": json.load(response)}
                    break
                except urllib.error.HTTPError as error:
                    if error.code not in (404, 429, 500, 502, 503, 504) or attempt == 5:
                        raise BillingError("Classifier billing lookup failed with HTTP " + str(error.code)) from None
                except (urllib.error.URLError, TimeoutError):
                    if attempt == 5:
                        raise BillingError("Classifier billing lookup remained unavailable") from None
                time.sleep(2)
            path.write_text(json.dumps(receipt, indent=2) + "\n")
            path.chmod(0o600)
        verify_zero_charge(failure, receipt)
        receipts.append(receipt)
    verify_receipts(failures, receipts)
    return receipts
