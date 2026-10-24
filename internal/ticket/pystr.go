package ticket

// Agents write reports, so a report value can be anything JSON holds. The
// asks read them as Python does (jsonx.Truthy, jsonx.PyStr): `x or y` takes
// y when x is falsy, and sentence() calls str() on whatever it gets.

// pyTruthyString is bool() of an optional string field.
func pyTruthyString(o Opt[string]) bool { return o.Or("") != "" }
