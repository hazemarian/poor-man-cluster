package i18n

// Templates receive numbers in whatever type their source struct used: docker's
// Info.CPUs is an int, JSON payloads decode into int64, byte counters can be
// uint64, and ratios arrive as float64. The template helper funcs take every
// number through this single coercion point, so a page cannot fail at render
// time with "expected int64; got int" depending on which code path fed it —
// which is exactly how the overview page first broke once real cluster data
// started flowing down the full-page path instead of the htmx fragment.

// Num coerces any numeric value a template might hold into int64.
func Num(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// Float coerces any numeric value a template might hold into float64.
func Float(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	default:
		return float64(Num(v))
	}
}
