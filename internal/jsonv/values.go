package jsonv

import "math"

// MaxSafeInteger is 2^53 − 1, the largest integer every JSON reader holds exactly
// (Number.MAX_SAFE_INTEGER).
const MaxSafeInteger = 1<<53 - 1

// AsObject returns v as an object (not null, not an array).
func AsObject(v any) (*Object, bool) {
	o, ok := v.(*Object)
	return o, ok && o != nil
}

// AsArray returns v as an array.
func AsArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

// AsString returns v as a string.
func AsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// AsNumber returns v as a number (a boolean is never one).
func AsNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// IsInteger tells whether v is a number whose value is an integer (Number.isInteger).
func IsInteger(v any) bool {
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f)
}

// AsSafeInteger returns v as an integer when it is a number whose value is an integer from
// −(2^53 − 1) to 2^53 − 1 (Number.isSafeInteger), whatever its spelling.
func AsSafeInteger(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || !IsSafeIntegerFloat(f) {
		return 0, false
	}
	return int64(f), true
}

// IsSafeIntegerFloat tells whether f is an integer from −(2^53 − 1) to 2^53 − 1.
func IsSafeIntegerFloat(f float64) bool {
	return f == math.Trunc(f) && math.Abs(f) <= MaxSafeInteger
}

// Field returns the member `name` of v when v is an object that has it.
func Field(v any, name string) (any, bool) {
	o, ok := AsObject(v)
	if !ok {
		return nil, false
	}
	return o.Get(name)
}
