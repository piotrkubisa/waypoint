package mapper

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/hashicorp/go-hclog"
)

// ChainTarget finds a Chain that results in the result satisfying the check
// function given a set of functions and input values. This will return nil
// if no valid chain can be found.
func ChainTarget(check CheckFunc, mappers []*Func, values ...interface{}) *Chain {
	for _, m := range mappers {
		// If this mapper doesn't result in a valid output, ignore it.
		if !check(m.Out) {
			continue
		}

		// We have a valid output, let's see if we can build a chain.
		chain, err := m.Chain(mappers, values...)
		if err == nil {
			return chain
		}
	}

	return nil
}

// NOTE(mitchellh): The whole algorithm below is sub-optimal in many ways:
// we use too much state, we duplicate processing work, etc. We can improve
// this as needed since the tests are written and are high level.

// Chain is similar to Prepare or Call but takes a list of Funcs that
// can be called as intermediaries to convert parameters to the expected
// parameters for this func. The result is a "Chain" of function calls.
func (f *Func) Chain(mappers []*Func, values ...interface{}) (*Chain, error) {
	log := f.Logger.With("func", f.String())

	// Add any extra values
	values = append(values, f.Values...)

	// If we're logging, then build up a more log-friendly view of values.
	var valueTypes []string
	if log.IsTrace() {
		for _, v := range values {
			valueTypes = append(valueTypes, fmt.Sprintf("%T", v))
		}
		sort.Strings(valueTypes)
	}
	log.Trace("creating chain", "values", valueTypes)

	// First, we need to determine what we're missing for our func.
	missing := make(map[Type]int)
	f.args(values, missing)

	// If we're not missing anything then short-circuit the whole thing
	if len(missing) == 0 {
		log.Trace("chain satisfied by inputs")
		return &Chain{funcs: []*Func{f}, values: values}, nil
	}
	if log.IsTrace() {
		for t, idx := range missing {
			log.Trace("missing argument", "type", t.String(), "idx", idx)
		}
	}
	missing = nil // We don't need this anymore

	// Build a map of what our functions all provide
	mapperByOut := make(map[interface{}][]*Func)
	for _, m := range mappers {
		mapperByOut[m.Out.Key()] = append(mapperByOut[m.Out.Key()], m)
		log.Trace("available mapper", "in_types", m.Args, "out_type", m.Out.String(), "out_key", m.Out.Key())
	}

	// Build our chain
	chain, err := f.chain(
		log,
		values,
		mapperByOut,
		make([]*Func, 0, 1),
		make(map[*Func]struct{}),
		make(map[*Func]struct{}),
	)
	if err != nil {
		log.Trace("chain not found, DEBUG and lower level has search information")
		return nil, err
	}

	log.Trace("chain satisfied by mappers", "chain", chain)
	return &Chain{funcs: chain, values: values}, nil
}

// ChainInputSet returns the list of types that must be satisfied in order to
// call this function. The mappers are candidates to be called to perform
// type conversion along the way. And the check function will be called for
// the various inputs to check if it can be satisfied by the caller.
//
// For check, the caller should return true if the caller can produce a
// value to match that type or false if not.
//
// The result is a list of types that when satisfied will result in a
// guaranteed callable Func.
func (f *Func) ChainInputSet(mappers []*Func, check func(Type) bool) []Type {
	f.Logger.Trace("available mappers", "len", len(mappers))
	for _, m := range mappers {
		f.Logger.Trace("available mapper", "in_types", m.Args, "out_type", m.Out.String(), "out_key", m.Out.Key())
	}

	typeMap := f.inputSet(mappers, check, map[*Func]struct{}{}, nil)
	if typeMap == nil {
		return nil
	}

	result := make([]Type, 0, len(typeMap))
	for _, typ := range typeMap {
		result = append(result, typ)
	}

	return result
}

// NOTE(mitchellh): this is probably better suited for solving via
// SAT or something similar. In practice our sets are small enough that
// this search is probably cheap enough.
func (f *Func) inputSet(
	mappers []*Func,
	check func(Type) bool,
	visited map[*Func]struct{},
	input map[interface{}]Type,
) map[interface{}]Type {
	missing := make(map[interface{}]Type)
	pending := make(map[interface{}]Type)
	for k, v := range input {
		pending[k] = v
	}

	// Go through each argument type we expect
	for _, arg := range f.Args {
		key := arg.Key()

		// If we're already expecting this type it means we can satisfy it
		if _, ok := pending[key]; ok {
			continue
		}

		// If we can't satisfy it, then this is something we need a mapper for.
		if !check(arg) {
			missing[arg.Key()] = arg
			continue
		}

		// We can satisfy this type, add it to the pending result
		pending[key] = arg
	}

	// If we satisfied everything, then we're good!
	if len(missing) == 0 {
		return pending
	}

	// We have missing values
	for _, t := range missing {
		f.Logger.Trace("missing argument", "func", f.String(), "type", t.String())
	}

	// We're missing some values, let's see if the mappers can get us there.
	for _, m := range mappers {
		// If we already visited this mapper, ignore it to avoid cycles
		// as well as repeated work.
		if _, ok := visited[m]; ok {
			continue
		}

		key := m.Out.Key()

		// If our output type is not in the missing map, then we don't
		// need it so we can just skip it.
		if _, ok := missing[key]; !ok {
			continue
		}

		log := f.Logger.With("func", m.String(), "func_out", m.Out.String())
		log.Trace("func may satisfy missing argument")

		// We need this type! Let's see if we satisfy everything.
		// We also mark this as visited so that in the future we never
		// attempt to use this mapper again. We delete the visited marker
		// right after because we only don't want to visit again in the same
		// chain.
		visited[m] = struct{}{}
		result := m.inputSet(mappers, check, visited, pending)
		delete(visited, m)
		if result == nil {
			// Failed to find a path.
			log.Trace("func did NOT satisfy requirements")
			continue
		}

		// We found a path! Delete the missing type since it is now satisfied
		delete(missing, key)

		log.Trace("missing argument satisfied")

		// If we are out of missing values, then we found a path!
		if len(missing) == 0 {
			return result
		}

		// Accumulate
		pending = result
	}

	if input == nil {
		for _, t := range missing {
			f.Logger.Trace("final missing argument", "type", t.String())
		}
	}

	// If we made it here it means that we couldn't find a path since
	// the loop above will return early if we find the path.
	return nil
}

// chain is the internal recursive functions called on functions to build
// up the chain.
func (f *Func) chain(
	log hclog.Logger,
	values []interface{},
	mapperByOut map[interface{}][]*Func, // mappers by output type
	chain []*Func, // chain so far
	chainSet map[*Func]struct{}, // set of functions we're calling so far
	pendingSet map[*Func]struct{}, // stack of functions that aren't yet satisfied
) ([]*Func, error) {
	missing := make(map[Type]int)
	f.args(values, missing)

	// If we have no missing values, we're satisfied
	if len(missing) == 0 {
		chainSet[f] = struct{}{}
		return append(chain, f), nil
	}

	// Add ourselves immediately to the pending set since we're no longer valid
	pendingSet[f] = struct{}{}
	defer delete(pendingSet, f)

MISSING_LOOP:
	// Go through each missing value and look for a func that will produce it
	for t, _ := range missing {
		ms := mapperByOut[t.Key()]
		if len(ms) > 0 {
			// See if we call any of these mappers already. If we do, then
			// we're satisfied by that and we can skip this missing value.
			for _, m := range ms {
				if _, ok := chainSet[m]; ok {
					continue MISSING_LOOP
				}
			}

			// Not satisfied yet so we go through each mapper and try to find
			// one that can be satisfied by our inputs.
			for _, m := range ms {
				// Skip any mappers in the pending set, since those are still
				// trying to be satisfied and if we tried to call it we'd
				// loop.
				if _, ok := pendingSet[m]; ok {
					continue
				}

				nextChain, err := m.chain(log, values, mapperByOut, chain, chainSet, pendingSet)
				if err == nil {
					log.Trace("missing argument successfully satisfied by mapper",
						"type", t.String(),
					)

					// Satisfied!
					chain = nextChain
					continue MISSING_LOOP
				}
			}
		}

		return nil, fmt.Errorf("unable to map to %s", t.String())
	}

	return append(chain, f), nil
}

// Chain represents a chain of functions that need to be called to build
// values to satisfy the inputs of the subsequent functions.
type Chain struct {
	// funcs is an ordered list of functions that need to be called.
	funcs []*Func

	// values is the list of values we have
	values []interface{}
}

// Out returns the output type for the chain.
func (c *Chain) Out() Type {
	return c.funcs[len(c.funcs)-1].Out
}

// Call calls all the functions in the chain and returns the first error
// or final result.
func (c *Chain) Call() (interface{}, error) {
	var result interface{}
	var err error
	for _, f := range c.funcs {
		result, err = f.Prepare(c.values...).Call()
		if err != nil {
			return nil, err
		}

		v := reflect.ValueOf(result)
		if v.IsValid() {
			c.values = append(c.values, result)
		}
	}

	return result, nil
}

// String implements Stringer and outputs a human-friendly description
// of the call chain that this represents.
func (c *Chain) String() string {
	ss := make([]string, len(c.funcs))
	for i, f := range c.funcs {
		ss[i] = f.String()
	}

	return strings.Join(ss, " => ")
}