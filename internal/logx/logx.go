// Package logx is where the package's warnings go (SPEC.md §4: warnings only, never a secret).
package logx

// Logger takes the package's warnings.
type Logger interface {
	Warn(message string)
}

// Func is a Logger that is a function.
type Func func(message string)

// Warn hands the warning to f.
func (f Func) Warn(message string) { f(message) }

// Nop drops every warning.
var Nop Logger = Func(func(string) {})
