package engine

import (
	"net/http"
	"time"

	painlessscript "github.com/shibukawa/painlessscript-go"
)

// Painless script execution, backed by github.com/shibukawa/painlessscript-go
// (a dependency-free, read-only interpreter). The library has no mutation
// context (no ctx._source writes, no scripted_metric state), so update
// scripts, update_by_query/reindex scripts and scripted_metric aggregations
// stay unsupported; everything that only reads a document and returns a
// value (script_fields, the script and script_score queries, the
// script_score function, and sort by _script) runs for real.

// docFieldReader adapts one osmem document to painlessscript.FieldReader,
// backed by the same fieldValues resolution sort and function_score use. A
// script that reads both doc['f'].size() and doc['f'].value resolves and
// fetches the field once, not twice.
type docFieldReader struct {
	ix    *Index
	d     *Doc
	cache map[string]docFieldValues
}

type docFieldValues struct {
	vals []any
	f    *Field // nil for an unmapped field
}

func (r *docFieldReader) fetch(field string) docFieldValues {
	if c, ok := r.cache[field]; ok {
		return c
	}
	f, _, _ := r.ix.Mapping.resolve(field)
	c := docFieldValues{vals: r.ix.fieldValues(r.d, field), f: f}
	if r.cache == nil {
		r.cache = map[string]docFieldValues{}
	}
	r.cache[field] = c
	return c
}

func (r *docFieldReader) FieldSize(field string) (int, error) {
	return len(r.fetch(field).vals), nil
}

func (r *docFieldReader) FieldValue(field string) (painlessscript.Value, error) {
	c := r.fetch(field)
	if len(c.vals) == 0 {
		return painlessscript.Value{}, &fieldValueError{field: field}
	}
	return scriptValueOf(c.f, field, c.vals[0])
}

type fieldValueError struct{ field string }

func (e *fieldValueError) Error() string { return "no value present for field [" + e.field + "]" }

// scriptValueOf converts one value from fieldValues (the type produced by
// convertValue: float64, string, bool, time.Time, [2]float64, ...) into a
// script Value. f is the field's mapping (nil when unmapped); integral field
// types keep Painless's long/double distinction (arithmetic on a long
// divides like an integer).
func scriptValueOf(f *Field, field string, v any) (painlessscript.Value, error) {
	switch t := v.(type) {
	case float64:
		if f != nil && f.isIntegral() {
			return painlessscript.Int64Value(int64(t)), nil
		}
		return painlessscript.Float64Value(t), nil
	case string:
		return painlessscript.StringValue(t), nil
	case bool:
		return painlessscript.BoolValue(t), nil
	case time.Time:
		return painlessscript.DateTimeValue(t), nil
	}
	return painlessscript.Value{}, &unsupportedFieldValueError{field: field}
}

type unsupportedFieldValueError struct{ field string }

func (e *unsupportedFieldValueError) Error() string {
	return "field [" + e.field + "] has a value type scripts do not support"
}

// convertScriptParams converts a script's params object into painlessscript
// values. GoValue only converts Go scalars (and time.Time); painlessscript
// has no public constructor for a host-supplied list or map value, so
// nested objects and arrays are rejected here rather than silently dropped.
func convertScriptParams(m M) (map[string]painlessscript.Value, *Error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]painlessscript.Value, len(m))
	for _, k := range keysSorted(m) {
		v := m[k]
		switch v.(type) {
		case M, []any:
			return nil, errIllegalArgument("script parameter [%s] must be a string, number or boolean; object and array script parameters are not supported by osmem", k)
		}
		val, err := painlessscript.GoValue(v)
		if err != nil {
			return nil, errIllegalArgument("script parameter [%s]: %v", k, err)
		}
		out[k] = val
	}
	return out, nil
}

// compile compiles sc for pctx, memoized so a script parsed once for a
// request is compiled (and its params converted) at most once regardless of
// how many documents or indices it runs against.
func (sc *scriptSpec) compile(pctx painlessscript.Context) (*painlessscript.Program, map[string]painlessscript.Value, *Error) {
	sc.compileOnce.Do(func() {
		if sc.stored {
			sc.compileErr = &Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "unable to find script [" + sc.idOrCode + "] in cluster state"}
			return
		}
		if sc.lang != "painless" {
			sc.compileErr = errUnsupported("[" + sc.lang + "] scripts")
			return
		}
		params, perr := convertScriptParams(sc.params)
		if perr != nil {
			sc.compileErr = perr
			return
		}
		prog, cerr := painlessscript.Compile(sc.idOrCode, painlessscript.CompileOptions{Context: pctx})
		if cerr != nil {
			sc.compileErr = errScriptException("compile error", sc, asPainlessError(painlessscript.ErrorCompile, cerr))
			return
		}
		sc.program, sc.paramValues = prog, params
	})
	return sc.program, sc.paramValues, sc.compileErr
}

// evalDocScript runs sc's compiled program against one document. score is
// exposed to the script as _score (0 in contexts where it has no meaning).
func evalDocScript(sc *scriptSpec, pctx painlessscript.Context, ix *Index, d *Doc, score float64) (painlessscript.Value, *Error) {
	prog, params, cerr := sc.compile(pctx)
	if cerr != nil {
		return painlessscript.Value{}, cerr
	}
	v, err := prog.Eval(painlessscript.EvalContext{
		Params: params,
		Fields: &docFieldReader{ix: ix, d: d},
		Score:  painlessscript.Float64Value(score),
	})
	if err != nil {
		return painlessscript.Value{}, errScriptException("runtime error", sc, asPainlessError(painlessscript.ErrorRuntime, err))
	}
	return v, nil
}

// asPainlessError recovers the *painlessscript.Error a compile or Eval call
// returns (kept as error at the package boundary); library-internal errors
// are always this concrete type, but a foreign error (e.g. from a
// cancelled EvalContext.Context) is wrapped rather than asserted unchecked.
func asPainlessError(kind painlessscript.ErrorKind, err error) *painlessscript.Error {
	if pe, ok := err.(*painlessscript.Error); ok && pe != nil {
		return pe
	}
	return &painlessscript.Error{Kind: kind, Message: err.Error()}
}

// errScriptException mirrors ScriptException: a script_exception whose
// reason is "compile error" or "runtime error", wrapping the underlying
// cause. osmem does not reproduce Painless's real exception hierarchy, so
// the cause is always reported as illegal_argument_exception.
func errScriptException(reason string, sc *scriptSpec, perr *painlessscript.Error) *Error {
	status := http.StatusBadRequest
	if reason != "compile error" {
		status = http.StatusInternalServerError
	}
	extra := map[string]any{"script_stack": []string{sc.idOrCode}, "script": sc.idOrCode, "lang": sc.lang}
	if reason == "compile error" {
		extra["position"] = M{"offset": perr.Offset, "start": perr.Offset, "end": perr.Offset}
	}
	return &Error{Status: status, Type: "script_exception", Reason: reason, Extra: extra,
		Cause: &Error{Type: "illegal_argument_exception", Reason: perr.Message}}
}

// runScriptFields evaluates every script_fields entry for every hit,
// merging the results into hit.fields (the "fields" response key). A
// failing script fails the whole request unless the field ignores failures,
// matching ignore_failure (FetchSourcePhase/ScriptFieldsFetchSubPhase).
func (c *Cluster) runScriptFields(hits []*hit, specs []scriptFieldSpec) *Error {
	for _, h := range hits {
		for _, spec := range specs {
			v, err := evalDocScript(spec.script, painlessscript.ContextField, h.ix, h.doc, h.score)
			if err != nil {
				if spec.ignoreFailure {
					continue
				}
				return errSearchPhase(err)
			}
			if h.fields == nil {
				h.fields = M{}
			}
			h.fields[spec.name] = []any{scriptFieldOutput(v)}
		}
	}
	return nil
}

// scriptFieldOutput renders a script result the way script_fields and
// docvalue_fields render a value: Java-formatted numbers, one element.
func scriptFieldOutput(v painlessscript.Value) any {
	switch v.Kind() {
	case painlessscript.KindInt64:
		n, _ := v.Int64()
		return n
	case painlessscript.KindFloat64:
		f, _ := v.Float64()
		return Double(f)
	case painlessscript.KindString:
		s, _ := v.Text()
		return s
	case painlessscript.KindBool:
		b, _ := v.Bool()
		return b
	case painlessscript.KindDateTime:
		t, _ := v.Time()
		return t.Format(time.RFC3339Nano)
	}
	return nil
}
