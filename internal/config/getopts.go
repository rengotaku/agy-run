package config

import (
	"fmt"
	"strings"
)

// opt is a single parsed option occurrence (name plus its argument if any).
type opt struct {
	name byte
	arg  string
}

// longOpt maps a "--name" long option to the internal short byte the switch in
// Parse dispatches on, plus whether it takes an argument.
type longOpt struct {
	short    byte
	takesArg bool
}

// getopts is a small POSIX getopts-compatible parser. optspec is the classic
// spec string ("t:m:cC:w:da:h" — a letter followed by ":" takes an argument).
//
// It supports clustered flags without arguments (-cd), an argument attached to
// its flag (-t240) or in the next token (-t 240), and stops option scanning at
// the first non-option token or at "--" (matching shell getopts). Everything
// after that point is returned as rest.
//
// An optional long-option table (variadic so existing short-only callers are
// unaffected) recognizes "--name value" and "--name=value" forms, folding each
// onto its configured short byte so Parse dispatches uniformly.
func getopts(argv []string, optspec string, long ...map[string]longOpt) (opts []opt, rest []string, err error) {
	var longs map[string]longOpt
	if len(long) > 0 {
		longs = long[0]
	}

	takesArg := map[byte]bool{}
	valid := map[byte]bool{}
	for i := 0; i < len(optspec); i++ {
		c := optspec[i]
		valid[c] = true
		if i+1 < len(optspec) && optspec[i+1] == ':' {
			takesArg[c] = true
			i++
		}
	}

	i := 0
	for i < len(argv) {
		tok := argv[i]
		if tok == "--" {
			i++
			break
		}
		if len(tok) < 2 || tok[0] != '-' {
			break // first non-option: rest starts here
		}

		// Long option: "--name" or "--name=value".
		if strings.HasPrefix(tok, "--") {
			name := tok[2:]
			inline := ""
			hasInline := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				inline, hasInline = name[eq+1:], true
				name = name[:eq]
			}
			lo, ok := longs[name]
			if !ok {
				return nil, nil, fmt.Errorf("illegal option -- %s", name)
			}
			if lo.takesArg {
				switch {
				case hasInline:
					opts = append(opts, opt{name: lo.short, arg: inline})
				case i+1 < len(argv):
					i++
					opts = append(opts, opt{name: lo.short, arg: argv[i]})
				default:
					return nil, nil, fmt.Errorf("option requires an argument -- %s", name)
				}
			} else {
				opts = append(opts, opt{name: lo.short})
			}
			i++
			continue
		}

		// Parse a cluster of short flags within this token.
		j := 1
		for j < len(tok) {
			c := tok[j]
			if !valid[c] {
				return nil, nil, fmt.Errorf("illegal option -- %c", c)
			}
			if takesArg[c] {
				if j+1 < len(tok) {
					// attached argument: -t240
					opts = append(opts, opt{name: c, arg: tok[j+1:]})
				} else {
					// argument in next token: -t 240
					if i+1 >= len(argv) {
						return nil, nil, fmt.Errorf("option requires an argument -- %c", c)
					}
					i++
					opts = append(opts, opt{name: c, arg: argv[i]})
				}
				break // consumed the rest of this token
			}
			opts = append(opts, opt{name: c})
			j++
		}
		i++
	}

	rest = append(rest, argv[i:]...)
	return opts, rest, nil
}
