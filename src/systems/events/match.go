package events

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// EvalEvent reports whether the trigger filter m matches event e.
func EvalEvent(m Match, e Event) (bool, error) {
	ev, err := e.asMap()
	if err != nil {
		return false, err
	}
	return Eval(m, ev)
}

// Eval reports whether the filter m matches the decoded-JSON event ev. A group node
// (All/Any/Not) recurses; a leaf node tests the value at its Field with its Op. An empty
// filter matches everything (a trigger with no conditions fires on every event of its
// tenant), which is deliberate — narrowing is opt-in.
func Eval(m Match, ev map[string]any) (bool, error) {
	if m.isGroup() {
		for _, sub := range m.All { // AND
			ok, err := Eval(sub, ev)
			if err != nil || !ok {
				return false, err
			}
		}
		if len(m.Any) > 0 { // OR — at least one must hold
			matched := false
			for _, sub := range m.Any {
				ok, err := Eval(sub, ev)
				if err != nil {
					return false, err
				}
				if ok {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}
		}
		if m.Not != nil { // NOT
			ok, err := Eval(*m.Not, ev)
			if err != nil {
				return false, err
			}
			if ok {
				return false, nil
			}
		}
		return true, nil
	}
	if m.Field == "" {
		return true, nil // an empty filter (no group, no condition) matches every event
	}
	return evalLeaf(m, ev)
}

// evalLeaf applies one condition. A missing field fails closed for every operator except
// `exists`, which is the operator whose whole job is to test presence.
func evalLeaf(c Match, ev map[string]any) (bool, error) {
	got, present := lookup(c.Field, ev)

	switch c.Op {
	case "exists":
		want, ok := c.Value.(bool)
		if !ok {
			want = true // `exists` with no value means "must be present"
		}
		return present == want, nil
	}

	if !present {
		return false, nil
	}

	switch c.Op {
	case "", "eq":
		return equal(got, c.Value), nil
	case "ne":
		return !equal(got, c.Value), nil
	case "in":
		return inSet(got, c.Values), nil
	case "not_in":
		return !inSet(got, c.Values), nil
	case "prefix":
		return strings.HasPrefix(toStr(got), toStr(c.Value)), nil
	case "suffix":
		return strings.HasSuffix(toStr(got), toStr(c.Value)), nil
	case "glob":
		ok, err := path.Match(toStr(c.Value), toStr(got))
		if err != nil {
			return false, fmt.Errorf("glob %q: %w", c.Value, err)
		}
		return ok, nil
	case "regex":
		re, err := regexp.Compile(toStr(c.Value))
		if err != nil {
			return false, fmt.Errorf("regex %q: %w", c.Value, err)
		}
		return re.MatchString(toStr(got)), nil
	case "contains":
		return contains(got, c.Value), nil
	case "gt", "gte", "lt", "lte":
		return compareNum(c.Op, got, c.Value)
	default:
		return false, fmt.Errorf("unknown operator %q", c.Op)
	}
}

// lookup walks a dotted path through nested JSON objects, returning the value and whether
// the full path was present. Only object traversal is supported (arrays are matched whole
// via `contains`), which keeps filters predictable.
func lookup(field string, ev map[string]any) (any, bool) {
	if field == "" {
		return nil, false
	}
	var cur any = ev
	for _, seg := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[seg]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// equal compares an extracted value with a condition operand, coercing across the small set
// of JSON scalar types: numerics compare numerically (so 3 == 3.0), everything else compares
// by string form (so "dev" == "dev", true == true).
func equal(a, b any) bool {
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return toStr(a) == toStr(b)
}

func inSet(v any, set []any) bool {
	for _, s := range set {
		if equal(v, s) {
			return true
		}
	}
	return false
}

// contains: substring for strings, element membership for arrays.
func contains(got, want any) bool {
	if arr, ok := got.([]any); ok {
		return inSet(want, arr)
	}
	return strings.Contains(toStr(got), toStr(want))
}

func compareNum(op string, got, want any) (bool, error) {
	gf, gok := toFloat(got)
	wf, wok := toFloat(want)
	if !gok || !wok {
		return false, nil // non-numeric operands fail closed rather than erroring the whole dispatch
	}
	switch op {
	case "gt":
		return gf > wf, nil
	case "gte":
		return gf >= wf, nil
	case "lt":
		return gf < wf, nil
	case "lte":
		return gf <= wf, nil
	}
	return false, nil
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// toFloat coerces JSON numerics (decoded as float64) and numeric strings to a float.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}
