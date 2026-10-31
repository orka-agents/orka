package storagefault

import (
	"context"
	"fmt"
	"slices"
	"sync"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Injector applies faults to the calls of the providers it wraps and counts
// those calls. It is safe for concurrent use.
type Injector struct {
	mu     sync.Mutex
	rules  []*Rule
	counts map[callKey]*Counts
}

// New returns an injector with no faults.
func New() *Injector {
	return &Injector{counts: map[callKey]*Counts{}}
}

type callKey struct {
	contract string
	method   string
}

// Counts reports the calls a wrapped provider received.
type Counts struct {
	// Calls is the number of calls made to the wrapper, including calls a
	// fault refused.
	Calls int
	// Delivered is the number of calls that reached the wrapped provider.
	Delivered int
}

// Match selects calls. An empty field matches every call.
type Match struct {
	// Contract is the contract a call is made through, storagev1.StateContract
	// or storagev1.ArtifactsContract. Describe and HealthCheck are made through
	// the binding of the wrapper that receives them; the embedded families are
	// state calls.
	Contract string
	// Methods lists method names.
	Methods []string
	// Kinds lists method kinds from the storage method catalog. A method
	// outside the catalog, such as an embedded family's, matches no kind.
	Kinds []storagev1.MethodKind
	// Classes lists the call classes the storage method catalog assigns, such
	// as storagev1.CallBulk. A method outside the catalog matches no class.
	Classes []storagev1.CallClass
}

func (m Match) matches(contract, method string) bool {
	if m.Contract != "" && m.Contract != contract {
		return false
	}
	if len(m.Methods) > 0 && !slices.Contains(m.Methods, method) {
		return false
	}
	if len(m.Kinds) > 0 || len(m.Classes) > 0 {
		info, ok := storagev1.LookupMethod(method)
		if !ok {
			return false
		}
		if len(m.Kinds) > 0 && !slices.Contains(m.Kinds, info.Kind) {
			return false
		}
		if len(m.Classes) > 0 && !slices.Contains(m.Classes, info.Class) {
			return false
		}
	}
	return true
}

// clone returns m with its own copies of the lists, so a caller changing them
// later does not change a rule.
func (m Match) clone() Match {
	m.Methods = slices.Clone(m.Methods)
	m.Kinds = slices.Clone(m.Kinds)
	m.Classes = slices.Clone(m.Classes)
	return m
}

// Fault is one fault to inject.
type Fault struct {
	// Match selects the calls the fault affects.
	Match
	// Action is what a matching call does instead of, or around, reaching the
	// provider.
	Action Action
	// Times is how many matching calls the fault affects before it removes
	// itself. Zero means every matching call until the fault is removed.
	Times int
}

// Rule is an injected fault or Describe change. It stays in effect until
// Remove, or, for a fault with Times set, until it has affected that many
// calls.
type Rule struct {
	injector *Injector
	match    Match
	action   Action
	modify   func(*storagev1.ProviderInfo)
	times    int
	hits     int
}

// Remove ends the rule. Calls already affected by it, such as calls a hang
// holds, are not changed.
func (r *Rule) Remove() {
	r.injector.mu.Lock()
	defer r.injector.mu.Unlock()
	r.injector.remove(r)
}

// Hits returns how many calls the rule has affected.
func (r *Rule) Hits() int {
	r.injector.mu.Lock()
	defer r.injector.mu.Unlock()
	return r.hits
}

// remove deletes r from the injector's rules. The caller holds i.mu.
func (i *Injector) remove(r *Rule) {
	i.rules = slices.DeleteFunc(i.rules, func(rule *Rule) bool { return rule == r })
}

// Inject adds a fault. When several faults match a call, the one added last
// applies.
func (i *Injector) Inject(fault Fault) *Rule {
	if fault.Action == nil {
		panic("storagefault: a fault needs an action")
	}
	if fault.Times < 0 {
		panic("storagefault: a fault cannot affect a negative number of calls")
	}
	return i.add(&Rule{injector: i, match: fault.clone(), action: fault.Action, times: fault.Times})
}

func (i *Injector) add(rule *Rule) *Rule {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rules = append(i.rules, rule)
	return rule
}

// Down makes every call made through contract fail with
// storagev1.ErrUnavailable without reaching the provider, as if the store
// were unreachable, until the returned rule is removed.
func (i *Injector) Down(contract string) *Rule {
	return i.Inject(Fault{Match: Match{Contract: contract}, Action: Fail(storagev1.ErrUnavailable)})
}

// ModifyDescribe changes what Describe reports through contract, or through
// every contract when contract is empty, until the returned rule is removed.
// modify receives a copy of the provider's report. Changes apply in the order
// they were added, after any fault, and only to a Describe the provider
// answered.
func (i *Injector) ModifyDescribe(contract string, modify func(*storagev1.ProviderInfo)) *Rule {
	if modify == nil {
		panic("storagefault: ModifyDescribe needs a function")
	}
	return i.add(&Rule{injector: i, match: Match{Contract: contract, Methods: []string{"Describe"}}, modify: modify})
}

// SetIncarnation makes Describe report incarnation through contract, as after
// a provider restart or failover, until the returned rule is removed.
func (i *Injector) SetIncarnation(contract, incarnation string) *Rule {
	return i.ModifyDescribe(contract, func(info *storagev1.ProviderInfo) { info.Incarnation = incarnation })
}

// SetDatasetID makes Describe report datasetID through contract, as when the
// provider serves another dataset, until the returned rule is removed.
func (i *Injector) SetDatasetID(contract, datasetID string) *Rule {
	return i.ModifyDescribe(contract, func(info *storagev1.ProviderInfo) { info.DatasetID = datasetID })
}

// Counts returns the calls m matches, summed.
func (i *Injector) Counts(m Match) Counts {
	i.mu.Lock()
	defer i.mu.Unlock()
	var total Counts
	for key, counts := range i.counts {
		if m.matches(key.contract, key.method) {
			total.Calls += counts.Calls
			total.Delivered += counts.Delivered
		}
	}
	return total
}

// ResetCounts sets every count to zero.
func (i *Injector) ResetCounts() {
	i.mu.Lock()
	defer i.mu.Unlock()
	clear(i.counts)
}

// Error is the error a call returns when a fault decided its outcome instead
// of the provider. It matches Err, such as storagev1.ErrUnavailable.
type Error struct {
	// Contract and Method identify the call.
	Contract string
	Method   string
	// Err is the injected outcome.
	Err error
}

func (e *Error) Error() string {
	return fmt.Sprintf("storagefault: %s %s: %v", e.Contract, e.Method, e.Err)
}

func (e *Error) Unwrap() error {
	return e.Err
}

// call runs one provider call through the faults. next calls the wrapped
// provider. It reports whether a fault decided the outcome, in which case the
// caller discards any results the provider returned.
func (i *Injector) call(ctx context.Context, contract, method string, next func(context.Context) error) (bool, error) {
	key := callKey{contract: contract, method: method}
	i.mu.Lock()
	i.countLocked(key).Calls++
	action := i.actionLocked(contract, method)
	i.mu.Unlock()

	deliver := func(ctx context.Context) error {
		i.mu.Lock()
		i.countLocked(key).Delivered++
		i.mu.Unlock()
		return next(ctx)
	}
	if action == nil {
		return false, deliver(ctx)
	}
	injected, err := action.apply(ctx, method, deliver)
	if injected {
		return true, &Error{Contract: contract, Method: method, Err: err}
	}
	return false, err
}

// describe runs Describe through the faults, then applies the Describe
// changes for contract to a report the provider answered.
func (i *Injector) describe(
	ctx context.Context, contract string, next func(context.Context) (storagev1.ProviderInfo, error),
) (storagev1.ProviderInfo, error) {
	var info storagev1.ProviderInfo
	injected, err := i.call(ctx, contract, "Describe", func(ctx context.Context) (err error) {
		info, err = next(ctx)
		return err
	})
	if injected {
		return storagev1.ProviderInfo{}, err
	}
	if err != nil {
		return info, err
	}
	i.mu.Lock()
	var modifiers []*Rule
	for _, rule := range i.rules {
		if rule.modify != nil && rule.match.matches(contract, "Describe") {
			rule.hits++
			modifiers = append(modifiers, rule)
		}
	}
	i.mu.Unlock()
	if len(modifiers) == 0 {
		return info, nil
	}
	info = copyInfo(info)
	for _, rule := range modifiers {
		rule.modify(&info)
	}
	return info, nil
}

// copyInfo returns info with its own copy of every slice and pointer, so a
// Describe change never writes into the report the provider returned. A test
// fails when ProviderInfo gains a reference field this does not copy.
func copyInfo(info storagev1.ProviderInfo) storagev1.ProviderInfo {
	info.Contracts = slices.Clone(info.Contracts)
	info.Capabilities = slices.Clone(info.Capabilities)
	if info.Usage != nil {
		usage := *info.Usage
		info.Usage = &usage
	}
	return info
}

// actionLocked returns the action of the newest fault that matches the call,
// counting the call against it, or nil. The caller holds i.mu.
func (i *Injector) actionLocked(contract, method string) Action {
	for _, rule := range slices.Backward(i.rules) {
		if rule.action == nil || !rule.match.matches(contract, method) {
			continue
		}
		rule.hits++
		if rule.times > 0 && rule.hits >= rule.times {
			i.remove(rule)
		}
		return rule.action
	}
	return nil
}

// countLocked returns the counts for key. The caller holds i.mu.
func (i *Injector) countLocked(key callKey) *Counts {
	counts, ok := i.counts[key]
	if !ok {
		counts = &Counts{}
		i.counts[key] = counts
	}
	return counts
}
