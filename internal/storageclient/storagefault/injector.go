package storagefault

import (
	"context"
	"fmt"
	"slices"
	"sync"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type Injector struct {
	mu     sync.Mutex
	rules  []*Rule
	counts map[callKey]*Counts
}

func New() *Injector {
	return &Injector{counts: map[callKey]*Counts{}}
}

type callKey struct {
	contract string
	method   string
}

type Counts struct {
	Calls     int
	Delivered int
}

type Match struct {
	Contract string
	Methods  []string
	Kinds    []storagev1.MethodKind
	Classes  []storagev1.CallClass
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

func (m Match) clone() Match {
	m.Methods = slices.Clone(m.Methods)
	m.Kinds = slices.Clone(m.Kinds)
	m.Classes = slices.Clone(m.Classes)
	return m
}

type Fault struct {
	Match
	Action Action
	Times  int
}

type Rule struct {
	injector *Injector
	match    Match
	action   Action
	modify   func(*storagev1.ProviderInfo)
	times    int
	hits     int
}

func (r *Rule) Remove() {
	r.injector.mu.Lock()
	defer r.injector.mu.Unlock()
	r.injector.remove(r)
}

func (r *Rule) Hits() int {
	r.injector.mu.Lock()
	defer r.injector.mu.Unlock()
	return r.hits
}

func (i *Injector) remove(r *Rule) {
	i.rules = slices.DeleteFunc(i.rules, func(rule *Rule) bool { return rule == r })
}

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

func (i *Injector) Down(contract string) *Rule {
	return i.Inject(Fault{Match: Match{Contract: contract}, Action: Fail(storagev1.ErrUnavailable)})
}

func (i *Injector) ModifyDescribe(contract string, modify func(*storagev1.ProviderInfo)) *Rule {
	if modify == nil {
		panic("storagefault: ModifyDescribe needs a function")
	}
	return i.add(&Rule{injector: i, match: Match{Contract: contract, Methods: []string{"Describe"}}, modify: modify})
}

func (i *Injector) SetIncarnation(contract, incarnation string) *Rule {
	return i.ModifyDescribe(contract, func(info *storagev1.ProviderInfo) { info.Incarnation = incarnation })
}

func (i *Injector) SetDatasetID(contract, datasetID string) *Rule {
	return i.ModifyDescribe(contract, func(info *storagev1.ProviderInfo) { info.DatasetID = datasetID })
}

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

func (i *Injector) ResetCounts() {
	i.mu.Lock()
	defer i.mu.Unlock()
	clear(i.counts)
}

type Error struct {
	Contract string
	Method   string
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("storagefault: %s %s: %v", e.Contract, e.Method, e.Err)
}

func (e *Error) Unwrap() error {
	return e.Err
}

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

func copyInfo(info storagev1.ProviderInfo) storagev1.ProviderInfo {
	info.Contracts = slices.Clone(info.Contracts)
	info.Capabilities = slices.Clone(info.Capabilities)
	if info.Usage != nil {
		usage := *info.Usage
		info.Usage = &usage
	}
	if info.Binding != nil {
		binding := *info.Binding
		info.Binding = &binding
	}
	return info
}

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

func (i *Injector) countLocked(key callKey) *Counts {
	counts, ok := i.counts[key]
	if !ok {
		counts = &Counts{}
		i.counts[key] = counts
	}
	return counts
}
