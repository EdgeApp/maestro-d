package hubd

// slowCount is a runFlow value: a slow driver step, then output.<v>++.
func slowCount(v string) string {
	return `{"commands": [{"tapOn": "X"}, {"evalScript": "${output.` + v + ` = (output.` + v + ` || 0) + 1}"}]}`
}
