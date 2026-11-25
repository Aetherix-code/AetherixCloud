package compose

import (
	"fmt"
	"os"
	"strings"
)

// Interpolate expands ${VAR}, ${VAR:-default}, ${VAR-default}, ${VAR:?msg},
// ${VAR?msg}, $VAR and $$ using vars (falling back to nothing: the process
// environment is NOT consulted, use InterpolateEnv for that).
func Interpolate(s string, vars map[string]string) (string, error) {
	lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	return interpolate(s, lookup)
}

// InterpolateEnv is Interpolate that falls back to the process environment.
func InterpolateEnv(s string, vars map[string]string) (string, error) {
	return interpolate(s, func(k string) (string, bool) {
		if v, ok := vars[k]; ok {
			return v, true
		}
		return os.LookupEnv(k)
	})
}

func interpolate(s string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			break
		}
		switch n := s[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i++
		case n == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("compose: unterminated ${ in %q", s[i:min(len(s), i+20)])
			}
			expr := s[i+2 : i+end]
			i += end
			val, err := expand(expr, lookup)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
		case isNameStart(n):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			v, _ := lookup(s[i+1 : j])
			b.WriteString(v)
			i = j - 1
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

func expand(expr string, lookup func(string) (string, bool)) (string, error) {
	for _, op := range []string{":-", ":?", "-", "?"} {
		if i := strings.Index(expr, op); i > 0 {
			name, arg := expr[:i], expr[i+len(op):]
			v, ok := lookup(name)
			switch op {
			case ":-":
				if !ok || v == "" {
					return arg, nil
				}
			case "-":
				if !ok {
					return arg, nil
				}
			case ":?":
				if !ok || v == "" {
					return "", fmt.Errorf("compose: required variable %s is missing: %s", name, arg)
				}
			case "?":
				if !ok {
					return "", fmt.Errorf("compose: required variable %s is missing: %s", name, arg)
				}
			}
			return v, nil
		}
	}
	v, _ := lookup(expr)
	return v, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }
