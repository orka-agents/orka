// Package workerclient implements the native worker's task-bound gateway reply
// transport. Brokered ACP executions use their authorized transaction instead.
package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
)

// Config contains controller-owned identity and a projected Pod token path.
// Raw environment tokens are deliberately not supported.
type Config struct {
	ControllerURL string
	Namespace     string
	TaskName      string
	TaskUID       string
	TokenFile     string
}

// ErrUnavailable classifies optional service or transport failures. During
// bootstrap callers may omit the tool, never treat this error as an origin grant.
var ErrUnavailable = errors.New("gateway reply service is unavailable")

type controllerError struct{ status int }

func (e *controllerError) Error() string        { return "gateway reply controller rejected the request" }
func (e *controllerError) Is(target error) bool { return target == ErrUnavailable && e.status >= 500 }

type Client struct {
	taskUID   string
	endpoint  string
	tokenFile string
	http      *http.Client
}

var _ tools.GatewayReplySender = (*Client)(nil)

func New(cfg Config) (*Client, error) {
	invalid := errors.New("gateway reply client configuration is invalid")
	u, err := url.Parse(cfg.ControllerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, invalid
	}
	for _, id := range []string{cfg.Namespace, cfg.TaskName, cfg.TaskUID} {
		if !validIdentity(id) || strings.ContainsAny(id, "/\\?#%") {
			return nil, invalid
		}
	}
	if strings.TrimSpace(cfg.TokenFile) == "" {
		return nil, invalid
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Never forward the projected credential through env proxies.
	return &Client{taskUID: cfg.TaskUID, endpoint: strings.TrimRight(u.String(), "/") + "/internal/v1/tasks/" + cfg.Namespace + "/" + cfg.TaskName + "/gateway-messages", tokenFile: cfg.TokenFile, http: &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// AuthenticateOrigin binds the exact Task UID without asking for message
// admission. A fast Pod may precede Job identity publication, so retry 503 only,
// at most five attempts within ten seconds. No failed response proves identity.
func (c *Client) AuthenticateOrigin(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		var origin struct {
			TaskUID string `json:"taskUID"`
		}
		err := c.request(ctx, http.MethodGet, c.endpoint+"/origin", nil, &origin)
		if err == nil {
			if origin.TaskUID != c.taskUID {
				return errors.New("gateway reply origin identity is invalid")
			}
			return nil
		}
		response, ok := errors.AsType[*controllerError](err)
		if !ok || response.status != http.StatusServiceUnavailable || attempt >= 4 {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ErrUnavailable
		case <-timer.C:
		}
	}
}

func (c *Client) Budget(ctx context.Context, requestID string) (tools.GatewayReplyBudget, error) {
	var budget tools.GatewayReplyBudget
	if !validIdentity(requestID) {
		return budget, errors.New("gateway reply request identity is invalid")
	}
	err := c.request(ctx, http.MethodGet, c.endpoint+"/budget?requestID="+url.QueryEscape(requestID), nil, &budget)
	if err == nil && (budget.Limit <= 0 || budget.Accepted < 0) {
		err = errors.New("gateway reply budget response is invalid")
	}
	return budget, err
}

func (c *Client) Enqueue(ctx context.Context, requestID, content string) (tools.GatewayReplyReceipt, error) {
	var receipt tools.GatewayReplyReceipt
	if !validIdentity(requestID) || !utf8.ValidString(content) || len(content) > protocol.MaxInterimTextBytes || strings.TrimSpace(protocol.SanitizeMessage(content, 0)) == "" {
		return receipt, errors.New("gateway reply request is invalid")
	}
	body, err := json.Marshal(struct {
		Content   string `json:"content"`
		RequestID string `json:"requestID"`
	}{content, requestID})
	if err != nil {
		return receipt, errors.New("gateway reply request is invalid")
	}
	if err = c.request(ctx, http.MethodPost, c.endpoint, body, &receipt); err != nil {
		return tools.GatewayReplyReceipt{}, err
	}
	if !validIdentity(receipt.DeliveryID) || !strings.HasPrefix(receipt.DeliveryID, "gdm-") {
		return tools.GatewayReplyReceipt{}, errors.New("gateway reply receipt is invalid")
	}
	switch receipt.Status {
	case "Pending", "Sending", "Delivered", "RetryScheduled", "Failed", "DeadLettered", "Expired":
	default:
		return tools.GatewayReplyReceipt{}, errors.New("gateway reply receipt is invalid")
	}
	return receipt, nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body []byte, result any) error {
	token, err := workerenv.ReadTokenFile(c.tokenFile, "projected worker token")
	if err != nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("gateway reply request is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w; admission outcome may be unknown", ErrUnavailable)
	}
	defer response.Body.Close() //nolint:errcheck
	// Never copy provider, authorization, or upstream body diagnostics to the tool.
	if response.StatusCode != http.StatusOK && (method != http.MethodPost || response.StatusCode != http.StatusAccepted) {
		cause := &controllerError{status: response.StatusCode}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		if readErr == nil && len(data) <= 4096 && protocol.ValidJSONUnicode(data) {
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &envelope) == nil && gatewayRejectionStatus(envelope.Error.Code) == response.StatusCode {
				if rejection := tools.NewGatewayReplyRejection(envelope.Error.Code, cause); rejection != nil {
					return rejection
				}
			}
		}
		return cause
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return fmt.Errorf("%w; admission outcome may be unknown", ErrUnavailable)
	}
	if len(data) > 4096 || !protocol.ValidJSONUnicode(data) {
		return errors.New("gateway reply controller response is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil {
		return errors.New("gateway reply controller response is invalid")
	}
	return nil
}

// A known code must agree with the controller's status; arbitrary bodies never
// convert a transport/backend failure into a definitive non-admission result.
func gatewayRejectionStatus(code string) int {
	switch code {
	case "interim_delivery_unsupported", "conflict":
		return http.StatusConflict
	case "limit_reached":
		return http.StatusTooManyRequests
	case "invalid_request":
		return http.StatusBadRequest
	case "too_large":
		return http.StatusRequestEntityTooLarge
	case "forbidden":
		return http.StatusForbidden
	case "unauthorized":
		return http.StatusUnauthorized
	case "not_found":
		return http.StatusNotFound
	case "unavailable":
		return http.StatusServiceUnavailable
	default:
		return 0
	}
}

func validIdentity(id string) bool {
	return id != "" && id == strings.TrimSpace(id) && len(id) <= protocol.MaxIdentityBytes && utf8.ValidString(id) && !strings.ContainsFunc(id, unicode.IsControl)
}
