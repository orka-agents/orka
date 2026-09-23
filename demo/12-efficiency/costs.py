"""Dated API-price estimate plus an explicit allocation of measured node capacity.

This consumes the verified gateway report, not overlapping Orka token totals.
Infrastructure input is {phase: {entries: [...], expectedCounts: {...}}}. Each
entry identifies one independently inventoried Pod and supplies component,
resource, seconds, nodeHourlyUsd, cpuShare, and memoryShare. Shares are requested
resources divided by the node's allocatable resources, not measured utilization.
All returned money and percentages are decimal strings, without cent rounding.
"""

from __future__ import annotations

import copy
from datetime import date
from decimal import Decimal, DecimalException, Inexact, localcontext

import classifier_billing

PHASES = ("baseline", "routed")
TOKEN_FIELDS = ("prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "reasoning_tokens")
COMPONENTS = ("vekil", "local-model", "compatibility-router", "controller", "provider-proxy",
              "scm-proxy", "workspace-publisher", "native-worker", "codex-runtime")
RATE_CARD = {
    "asOf": "2026-09-22", "currency": "USD",
    "hosted": {
        "provider": "GitHub Copilot", "model": "GPT-5.5",
        "inputPerMillionUsd": "5", "cachedInputPerMillionUsd": "0.50", "outputPerMillionUsd": "30",
        "maxPromptTokensPerRequest": 272000,
        "source": "https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing",
    },
    "classifier": {
        "provider": "Vercel AI Gateway", "model": "typesafe-ai/jev",
        "inputPerMillionUsd": "0.042", "cachedInputPerMillionUsd": "0.042", "outputPerMillionUsd": "0",
        "promotional": True, "promotionEnds": "2026-09-25",
        "source": "https://ai-gateway.vercel.sh/v1/models",
        "promotionSource": "https://vercel.com/ai-gateway/models/jev",
    },
    "nodeReference": {
        "region": "westus2", "sku": "Standard_DS2_v2", "operatingSystem": "Linux",
        "retailHourlyUsd": "0.114", "priceType": "Consumption",
        "source": "https://prices.azure.com/api/retail/prices?$filter=serviceName%20eq%20%27Virtual%20Machines%27%20and%20armRegionName%20eq%20%27westus2%27%20and%20armSkuName%20eq%20%27Standard_DS2_v2%27%20and%20priceType%20eq%20%27Consumption%27",
    },
}
METHODOLOGY = {
    "estimate": "Published-rate estimate assuming Copilot token-based billing; not an invoice or metered spend.",
    "api": "Prompt tokens include cached tokens. Bill uncached prompt, cached prompt, and output once; reasoning is already part of output. Do not add overlapping Orka usage.",
    "classifier": "Use Jev's published market rate for reported usage, including startup checks. The temporary free promotion is not used to inflate savings. Missing gateway usage remains missing; an exact generation receipt with zero market cost and zero billed tokens can establish a failed call's zero charge separately.",
    "allocation": "Allocate each node's hourly compute price 50% to CPU and 50% to memory, using the Pod's requested share of allocatable CPU and memory. This is an allocation assumption, not a billing measurement.",
    "window": "Include idle capacity within each supplied, lifetime-clipped phase interval. No estimate is made for off-window idle time or the full infrastructure bill.",
    "baseline": "The hosted-only service comparison excludes baseline local-model allocation because that model was installed to prepare the routed test. Actual allocated test capacity retains it. All other inventoried components remain included.",
    "localModel": "Qwen has zero API fees here. Its allocated node capacity is reported separately.",
    "percentChange": "100 × (routed − baseline) / baseline. Negative is a decrease; a zero baseline has no defined percentage.",
}


class CostError(ValueError):
    """An estimate cannot be supported by the supplied records."""


def require(condition: bool, message: str) -> None:
    if not condition:
        raise CostError(message)


def mapping(value, label: str) -> dict:
    require(isinstance(value, dict), "Missing or invalid " + label)
    return value


def count(value, label: str) -> int:
    require(type(value) is int and value >= 0, "Missing or invalid counter: " + label)
    return value


def number(value, label: str) -> Decimal:
    require(type(value) in (int, float, str, Decimal), "Missing or invalid number: " + label)
    try:
        result = Decimal(str(value))
    except DecimalException as exc:
        raise CostError("Invalid number: " + label) from exc
    require(result.is_finite() and result >= 0, "Nonfinite or negative number: " + label)
    return result


def usage(value, label: str = "usage") -> dict:
    value = mapping(value, label)
    result = {key: count(value.get(key), label + "." + key) for key in TOKEN_FIELDS}
    require(result["total_tokens"] == result["prompt_tokens"] + result["completion_tokens"],
            "Token total disagrees with input and output: " + label)
    require(result["cached_tokens"] <= result["prompt_tokens"], "Cached input exceeds prompt tokens: " + label)
    require(result["reasoning_tokens"] <= result["completion_tokens"], "Reasoning exceeds output tokens: " + label)
    return result


def request_usage(value, requests: int, label: str) -> dict:
    result = usage(value, label)
    require(bool(requests) == bool(result["total_tokens"]), "Request count and usage disagree: " + label)
    return result


def token_cost(value: dict, rates: dict) -> dict:
    """Return exact Decimal charges; cached input and reasoning are subsets."""
    value = usage(value)
    rates = mapping(rates, "token rates")
    prices = [number(rates.get(key), key) for key in
              ("inputPerMillionUsd", "cachedInputPerMillionUsd", "outputPerMillionUsd")]
    with localcontext() as context:
        context.prec = 50
        context.traps[Inexact] = True
        try:
            amounts = [Decimal(tokens) * price / Decimal(1000000) for tokens, price in zip(
                (value["prompt_tokens"] - value["cached_tokens"], value["cached_tokens"], value["completion_tokens"]), prices)]
            return dict(zip(("uncachedInputUsd", "cachedInputUsd", "outputUsd", "totalUsd"),
                            [*amounts, sum(amounts, Decimal(0))]))
        except DecimalException as exc:
            raise CostError("Token charge exceeds exact decimal calculation precision") from exc


def phase_api_cost(gateway: dict, rate_card: dict) -> dict:
    gateway = mapping(gateway, "gateway")
    destinations = mapping(gateway.get("destinations"), "priced destinations")
    for name, model, provider_type in (
        ("powerful", rate_card["hosted"]["model"].lower(), "copilot"),
        ("classifier", rate_card["classifier"]["model"], "typesafe-compatible"),
        ("lightweight", "qwen-3.5-2b", "openai-compatible"),
    ):
        destination = mapping(destinations.get(name), name + " destination")
        require(destination.get("model") == model and destination.get("providerType") == provider_type,
                "Gateway destination does not match the rate card: " + name)
    require(destinations["lightweight"].get("provider") == "local-aikit"
            and destinations["lightweight"].get("service"), "Zero API fee requires the local model service")
    roles = mapping(gateway.get("roles"), "gateway roles")
    require(set(roles) == {"hostedWorker", "cpuWorker", "coordinator"}, "Missing or unpriced gateway role")
    parsed = {}
    for name, role in roles.items():
        role = mapping(role, name)
        requests = count(role.get("requests"), name + ".requests")
        parsed[name] = request_usage(role.get("usage"), requests, name)
    require(roles["coordinator"]["requests"] == 0, "Coordinator requests need their own price accounting")

    # The published default GPT-5.5 rate is per request, not per phase total.
    operations = gateway.get("operations")
    require(isinstance(operations, list), "Missing per-request context-size evidence")
    grouped = {name: [] for name in roles}
    limit = count(rate_card["hosted"].get("maxPromptTokensPerRequest"), "hosted context threshold")
    require(limit > 0, "Invalid hosted context threshold")
    operation_ids = set()
    for operation in operations:
        operation = mapping(operation, "gateway operation")
        identity = operation.get("operationID")
        require(isinstance(identity, str) and identity and identity not in operation_ids,
                "Missing or duplicate gateway operation identity")
        operation_ids.add(identity)
        require(operation.get("role") == "worker" and operation.get("tier") in ("lightweight", "powerful"),
                "Unpriced gateway operation")
        require(operation.get("destination") == destinations[operation["tier"]], "Operation destination differs from the priced route")
        name = "hostedWorker" if operation["tier"] == "powerful" else "cpuWorker"
        item = request_usage(operation.get("usage"), 1, "operation usage")
        if name == "hostedWorker":
            require(item["prompt_tokens"] <= limit, "Hosted request exceeds the priced default-context tier")
        grouped[name].append(item)
    for name, rows in grouped.items():
        require(len(rows) == roles[name]["requests"] and
                {key: sum(row[key] for row in rows) for key in TOKEN_FIELDS} == parsed[name],
                "Gateway operations and role totals disagree: " + name)

    classifier = mapping(gateway.get("classifier"), "classifier")
    counters = {key: count(classifier.get(key), "classifier." + key) for key in
                ("sends", "completed", "errors", "throttled", "reported_usage_sends", "duration_ms")}
    failures = classifier.get("unmeteredFailures", [])
    receipts = classifier.get("billingReceipts", [])
    try:
        billed_failures = classifier_billing.verify_receipts(failures, receipts)
    except classifier_billing.BillingError as error:
        raise CostError(str(error)) from error
    require(counters["sends"] == counters["completed"] == counters["reported_usage_sends"] + len(billed_failures)
            and counters["errors"] == len(billed_failures) and counters["throttled"] == 0,
            "Incomplete classifier accounting")
    require(classifier.get("usageComplete") is (not billed_failures), "Classifier token usage is unavailable")
    fallback_ids = {row["operationID"] for row in operations
                    if row.get("policyDecision") == "unavailable_fallback"
                    and row.get("classifierFailureCategory") == "upstream_5xx"}
    require(len(fallback_ids) == len(failures) and fallback_ids == {row.get("operationID") for row in failures},
            "Failed classifier billing is not tied to the fallback operations")
    classifier_tokens = request_usage(classifier.get("usage"), counters["reported_usage_sends"], "reported classifier usage")
    preflights = gateway.get("preflightBeforeInterval")
    require(isinstance(preflights, list), "Missing classifier startup accounting")
    startup = []
    for row in preflights:
        row = mapping(row, "classifier startup")
        require(row.get("usageComplete") is True, "Classifier startup token usage is unavailable")
        sends = count(row.get("physical_classifier_sends"), "startup sends")
        require(sends > 0, "Empty classifier startup record")
        raw = mapping(row.get("classifier_usage"), "startup usage")
        item = request_usage({key: raw.get(source) for key, source in zip(TOKEN_FIELDS,
            ("input_tokens", "output_tokens", "total_tokens", "cached_input_tokens", "reasoning_tokens"))}, sends, "startup usage")
        startup.append({"requests": sends, "usage": item, "charge": token_cost(item, rate_card["classifier"])})
    hosted = token_cost(parsed["hostedWorker"], rate_card["hosted"])
    classified = token_cost(classifier_tokens, rate_card["classifier"])
    preflight = sum((row["charge"]["totalUsd"] for row in startup), Decimal(0))
    return {"hostedUsd": hosted["totalUsd"], "classifierUsd": classified["totalUsd"],
            "preflightUsd": preflight, "localModelUsd": Decimal(0),
            "subtotalUsd": hosted["totalUsd"] + classified["totalUsd"] + preflight,
            "hostedBreakdown": hosted, "classifierBreakdown": classified, "preflight": startup,
            "zeroChargeClassifierFailures": len(billed_failures)}


def infrastructure_cost(value: dict, phase: str) -> dict:
    require(phase in PHASES, "Unknown cost phase")
    value = mapping(value, phase + " infrastructure")
    expected = mapping(value.get("expectedCounts"), phase + " infrastructure expectedCounts")
    require(set(expected) == set(COMPONENTS), "Missing or unknown infrastructure component inventory")
    expected = {key: count(total, "expectedCount." + key) for key, total in expected.items()}
    require(all(expected[key] > 0 for key in COMPONENTS if key not in ("native-worker", "codex-runtime")),
            "An always-on demo component is absent from the inventory")
    entries = value.get("entries")
    require(isinstance(entries, list), "Missing infrastructure entries")
    seen, actual, rows = set(), dict.fromkeys(COMPONENTS, 0), []
    with localcontext() as context:
        context.prec = 50
        for entry in entries:
            entry = mapping(entry, "infrastructure entry")
            component, resource = entry.get("component"), entry.get("resource")
            require(component in COMPONENTS, "Missing or unknown infrastructure component")
            require(isinstance(resource, str) and bool(resource.strip()) and resource not in seen,
                    "Missing or duplicate infrastructure resource identity")
            seen.add(resource)
            actual[component] += 1
            seconds, hourly, cpu, memory = [number(entry.get(key), key) for key in
                ("seconds", "nodeHourlyUsd", "cpuShare", "memoryShare")]
            require(seconds > 0 and hourly > 0, "Infrastructure duration and hourly price must be positive")
            require(cpu <= 1 and memory <= 1 and cpu + memory > 0, "Invalid requested share of node capacity")
            share = Decimal("0.5") * cpu + Decimal("0.5") * memory
            amount = hourly * share * seconds / Decimal(3600)
            excluded = amount if phase == "baseline" and component == "local-model" else Decimal(0)
            rows.append({"component": component, "resource": resource, "seconds": seconds,
                         "nodeHourlyUsd": hourly, "cpuShare": cpu, "memoryShare": memory,
                         "weightedShare": share, "actualAllocatedUsd": amount,
                         "excludedPreparationUsd": excluded, "allocatedUsd": amount - excluded})
        require(actual == expected, "Infrastructure counts do not match the independent inventory")
        return {"weights": {"cpu": "0.5", "memory": "0.5"}, "expectedCounts": expected, "entries": rows,
                "actualAllocatedTestCapacityUsd": sum((row["actualAllocatedUsd"] for row in rows), Decimal(0)),
                "excludedPreparationUsd": sum((row["excludedPreparationUsd"] for row in rows), Decimal(0)),
                "allocatedUsd": sum((row["allocatedUsd"] for row in rows), Decimal(0))}


def compare_costs(baseline, routed) -> dict:
    baseline, routed = number(baseline, "baseline cost"), number(routed, "routed cost")
    with localcontext() as context:
        context.prec = 50
        delta = routed - baseline
        return {"baselineUsd": baseline, "routedUsd": routed, "deltaUsd": delta,
                "percentChange": delta * Decimal(100) / baseline if baseline else None,
                "percentageUnavailableReason": None if baseline else "Baseline cost is zero."}


def json_values(value):
    if isinstance(value, Decimal):
        return format(value, "f")
    if isinstance(value, dict):
        return {key: json_values(item) for key, item in value.items()}
    if isinstance(value, list):
        return [json_values(item) for item in value]
    return value


def calculate_costs(verified_report: dict, infrastructure: dict, rate_card: dict = RATE_CARD) -> dict:
    """Return dated, JSON-safe estimates; reject incomplete usage or inventory."""
    report = mapping(verified_report, "verified report")
    phases = mapping(report.get("phases"), "verified phases")
    require(set(phases) == set(PHASES), "Both comparison phases are required")
    infrastructure = mapping(infrastructure, "infrastructure")
    require(set(infrastructure) == set(PHASES), "Both infrastructure phases are required")
    rate_card = copy.deepcopy(mapping(rate_card, "rate card"))
    require(rate_card.get("currency") == "USD", "Rate card must use USD")
    try:
        observed = date.fromisoformat(rate_card["asOf"])
        promotion_ends = date.fromisoformat(rate_card["classifier"]["promotionEnds"])
    except (KeyError, TypeError, ValueError) as exc:
        raise CostError("Rate card needs its observation and promotion dates") from exc
    for kind in ("hosted", "classifier"):
        mapping(rate_card.get(kind), kind + " rates")
        require(isinstance(rate_card[kind].get("model"), str) and rate_card[kind]["model"], "Missing rate-card model")
        require(isinstance(rate_card[kind].get("source"), str) and rate_card[kind]["source"].startswith("https://"),
                "Rate card is missing a primary source")
    require(observed <= promotion_ends, "Rate-card observation is after the classifier promotion ends")
    result = {"rateCard": rate_card, "methodology": copy.deepcopy(METHODOLOGY), "phases": {}}
    with localcontext() as context:
        context.prec = 50
        for name in PHASES:
            phase = mapping(phases[name], name + " phase")
            api = phase_api_cost(phase.get("gateway"), rate_card)
            allocation = infrastructure_cost(infrastructure[name], name)
            result["phases"][name] = {"api": api, "infrastructure": allocation,
                "combinedUsd": api["subtotalUsd"] + allocation["allocatedUsd"],
                "actualTestCombinedUsd": api["subtotalUsd"] + allocation["actualAllocatedTestCapacityUsd"]}
        a, b = [result["phases"][name] for name in PHASES]
        result["comparison"] = {
            "api": compare_costs(a["api"]["subtotalUsd"], b["api"]["subtotalUsd"]),
            "combined": compare_costs(a["combinedUsd"], b["combinedUsd"]),
            "actualTestCombined": compare_costs(a["actualTestCombinedUsd"], b["actualTestCombinedUsd"]),
        }
    return json_values(result)


def format_summary(result: dict) -> str:
    """Compact terminal view; the returned report retains unrounded values."""
    a, b = [result["phases"][name] for name in PHASES]

    def row(label, first, second):
        return f"{label:<28} ${Decimal(first):>12.4f}  ${Decimal(second):>12.4f}"

    def change(label, values):
        percent = "n/a (zero baseline)" if values["percentChange"] is None else f"{Decimal(values['percentChange']):+.1f}%"
        return f"{label:<28} ${Decimal(values['deltaUsd']):+.4f}  ({percent})"

    lines = [f"Estimated USD, rate card {result['rateCard']['asOf']}",
             f"{'':28} {'Hosted baseline':>15}  {'Routing enabled':>15}"]
    for label, key in (("Hosted model", "hostedUsd"), ("Jev during jobs", "classifierUsd"),
                       ("Jev startup before jobs", "preflightUsd"), ("API subtotal", "subtotalUsd")):
        lines.append(row(label, a["api"][key], b["api"][key]))
    lines += [row("Allocated service capacity", a["infrastructure"]["allocatedUsd"], b["infrastructure"]["allocatedUsd"]),
              row("Combined service estimate", a["combinedUsd"], b["combinedUsd"]), "",
              change("API change", result["comparison"]["api"]),
              change("Combined estimate change", result["comparison"]["combined"]), "",
              row("Actual test capacity", a["infrastructure"]["actualAllocatedTestCapacityUsd"], b["infrastructure"]["actualAllocatedTestCapacityUsd"]),
              row("Actual test combined", a["actualTestCombinedUsd"], b["actualTestCombinedUsd"]),
              f"Baseline preparation excluded from service estimate: ${Decimal(a['infrastructure']['excludedPreparationUsd']):.4f}",
              "50% CPU + 50% RAM request allocation; measured-window idle included.",
              "Hosted-only baseline excludes preparatory local-model capacity; actual test totals retain it.",
              "Assumes token-based Copilot billing. This is not an invoice or an off-window estimate.",
              "Jev uses its published market rate; temporary free access is not assumed."]
    failures = sum(row["api"]["zeroChargeClassifierFailures"] for row in (a, b))
    if failures:
        lines.append(f"{failures} failed Jev calls have exact zero-charge billing receipts; token counts remain unavailable.")
    require(len(lines) <= 22 and all(len(line) <= 100 for line in lines), "Cost summary exceeds terminal dimensions")
    return "\n".join(lines) + "\n"
