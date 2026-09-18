package api

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

const (
	usageMonitorResource = "repositorymonitors"
	usageSessionResource = "sessions"
)

func (h *Handlers) GetUsageReport(c fiber.Ctx) error {
	report, err := h.usageReport(c, false)
	if err != nil {
		return err
	}
	return c.JSON(report)
}

func (h *Handlers) GetUsageWork(c fiber.Ctx) error {
	report, err := h.usageReport(c, true)
	if err != nil {
		return err
	}
	for _, work := range report.Works {
		if work.ID == c.Params("id") {
			return c.JSON(fiber.Map{"selection": report.Selection, "work": work})
		}
	}
	return fiber.NewError(fiber.StatusNotFound, "work request not found")
}

func (h *Handlers) usageReport(c fiber.Ctx, detail bool) (usage.Report, error) {
	var empty usage.Report
	if err := h.authorizeUsageContextToken(c); err != nil {
		return empty, err
	}
	backend, ok := h.executionEventStore.(store.UsageStore)
	if !ok {
		return empty, fiber.NewError(fiber.StatusNotImplemented, "usage reporting is not configured")
	}
	teams, err := h.usageTeams(c)
	if err != nil {
		return empty, err
	}
	filter, err := usageReportFilter(c, teams, detail)
	if err != nil {
		return empty, err
	}
	reader := h.uncachedReader()
	filter.NamespaceUIDs = make(map[string]string, len(teams))
	for _, team := range teams {
		uid, err := usage.NamespaceUID(c.Context(), reader, team)
		if err != nil {
			return empty, fiber.NewError(fiber.StatusServiceUnavailable, "failed to verify usage namespace identity")
		}
		filter.NamespaceUIDs[team] = uid
	}
	data, err := backend.LoadUsage(c.Context(), filter)
	if err != nil {
		return empty, fiber.NewError(fiber.StatusInternalServerError, "failed to load usage report")
	}
	if err := h.filterUsageTaskAccess(c, &data); err != nil {
		return empty, err
	}
	for _, team := range teams {
		uid, err := usage.NamespaceUID(c.Context(), reader, team)
		if err != nil || uid != filter.NamespaceUIDs[team] {
			return empty, fiber.NewError(fiber.StatusServiceUnavailable, "usage namespace identity changed")
		}
	}
	return usage.Build(data, filter), nil
}

func (h *Handlers) authorizeUsageContextToken(c fiber.Ctx) error {
	for _, scopes := range [][]string{h.contextTokenAuthorization.TaskListScopes, h.contextTokenAuthorization.MonitorReadScopes, h.contextTokenAuthorization.SessionReadScopes} {
		if err := h.authorizeContextTokenAction(c, "usageReport", scopes); err != nil {
			return err
		}
	}
	// A counts-only archive cannot reconstruct effective Agent/Provider/tool
	// policy after objects are deleted. Object-constrained tokens therefore
	// cannot authorize an aggregate, even with list scopes.
	if ui := GetUserInfo(c); h.contextTokenAuthorization.Enabled() && ui != nil && ui.AuthType == AuthTypeContextToken && ui.ContextToken != nil {
		for key := range ui.ContextToken.TransactionContext {
			if key != toolNamespaceArg {
				if err := h.handleContextTokenAuthorizationFailures(ui.ContextToken, "usageReport", []string{"usage reporting requires a token without object constraints"}); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (h *Handlers) usageTeams(c fiber.Ctx) ([]string, error) {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg))
	if err != nil {
		return nil, err
	}
	teams := []string{namespace}
	if raw := strings.TrimSpace(c.Query("teams")); raw != "" {
		teams = strings.Split(raw, ",")
		if len(teams) > 20 {
			return nil, fiber.NewError(fiber.StatusBadRequest, "at most 20 teams may be selected")
		}
		for i := range teams {
			teams[i] = strings.TrimSpace(teams[i])
			if teams[i] == "" {
				return nil, fiber.NewError(fiber.StatusBadRequest, "team namespace must not be empty")
			}
			if _, err := h.resolveNamespace(c, teams[i]); err != nil {
				return nil, err
			}
			for _, resource := range []string{externalToolTaskResource, usageMonitorResource, usageSessionResource} {
				if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), teams[i], "list", corev1alpha1.GroupVersion.Group, resource, ""); err != nil {
					return nil, err
				}
			}
		}
		slices.Sort(teams)
		teams = slices.Compact(teams)
	}
	if ui := GetUserInfo(c); h.contextTokenAuthorization.Enabled() && ui != nil && ui.AuthType == AuthTypeContextToken && ui.ContextToken != nil {
		if required, ok := contextString(ui.ContextToken.TransactionContext, toolNamespaceArg); ok {
			for _, team := range teams {
				if team != required {
					if err := h.handleContextTokenAuthorizationFailures(ui.ContextToken, "usageReport", []string{"namespace does not match token context"}); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return teams, nil
}

func usageReportFilter(c fiber.Ctx, teams []string, detail bool) (store.UsageFilter, error) {
	var empty store.UsageFilter
	now := time.Now().UTC()
	asOf, err := usageDate(c.Query("asOf"), now)
	if err != nil {
		return empty, err
	}
	if asOf.After(now.Add(time.Second)) {
		return empty, fiber.NewError(fiber.StatusBadRequest, "asOf must not be in the future")
	}
	start := time.Date(asOf.Year(), asOf.Month(), 1, 0, 0, 0, 0, time.UTC)
	if detail {
		start = time.Unix(0, 0).UTC()
	}
	from, err := usageDate(c.Query("from"), start)
	if err != nil {
		return empty, err
	}
	until, err := usageDate(c.Query("until"), asOf.Add(time.Nanosecond))
	if err != nil {
		return empty, err
	}
	if !until.After(from) {
		return empty, fiber.NewError(fiber.StatusBadRequest, "until must be after from")
	}
	kind := c.Query("kind")
	if kind != "" && kind != "issue" && kind != githubEventPullRequest {
		return empty, fiber.NewError(fiber.StatusBadRequest, "kind must be issue or pull_request")
	}
	return store.UsageFilter{Namespaces: teams, Repository: strings.TrimSpace(c.Query("repository")), Model: strings.TrimSpace(c.Query("model")),
		Kind: kind, From: from, Until: until, AsOf: asOf}, nil
}

func (h *Handlers) filterUsageTaskAccess(c fiber.Ctx, data *store.UsageData) error {
	data.HiddenTasks = map[string]bool{}
	known := map[string]bool{}
	cache := map[gatewayTaskAuthorizationKey]bool{}
	hiddenWorks := map[string]bool{}
	for _, task := range data.Tasks {
		key := task.Namespace + "/" + task.TaskUID
		known[key] = true
		owner := task.GatewayOwner
		if owner == nil {
			continue
		}
		identity := gatewayruntime.TaskOwnerIdentity{GatewayNamespace: owner.Namespace, NamespaceUID: owner.NamespaceUID, GatewayName: owner.Name, GatewayUID: owner.UID}
		cacheKey := gatewayTaskAuthorizationKey{GatewayNamespace: owner.Namespace, NamespaceUID: owner.NamespaceUID, GatewayName: owner.Name, GatewayUID: owner.UID}
		allowed, checked := cache[cacheKey]
		if !checked {
			err := h.taskAccess().authorizeGatewayTaskIdentity(c, "usageReport", &corev1alpha1.Task{}, identity, false)
			allowed = err == nil
			var status *fiber.Error
			if err != nil && (!errors.As(err, &status) || (status.Code != fiber.StatusForbidden && status.Code != fiber.StatusNotFound)) {
				return err
			}
			cache[cacheKey] = allowed
		}
		if !allowed {
			data.HiddenTasks[key] = true
			if task.WorkID != "" {
				hiddenWorks[task.WorkID] = true
			}
		}
	}
	// The controller registers Tasks before dispatch. An observation with no
	// retained Task identity has no verifiable access boundary yet.
	for _, observation := range data.Observations {
		key := observation.Namespace + "/" + observation.TaskUID
		if observation.TaskUID != "" && !known[key] {
			data.HiddenTasks[key] = true
		}
	}
	data.Works = slices.DeleteFunc(data.Works, func(work store.UsageWorkRequest) bool { return hiddenWorks[work.ID] })
	return nil
}

func usageDate(raw string, fallback time.Time) (time.Time, error) {
	if raw == "" {
		return fallback, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.DateOnly} {
		if value, err := time.Parse(layout, raw); err == nil {
			return value.UTC(), nil
		}
	}
	return time.Time{}, fiber.NewError(fiber.StatusBadRequest, "report dates must be YYYY-MM-DD or RFC3339 timestamps")
}
