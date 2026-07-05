package config

import "fmt"

// opt is a single parsed option occurrence (name plus its argument if any).
type opt struct {
	name byte
	arg  string
}

// getopts is a small POSIX getopts-compatible parser. optspec is the classic
// spec string ("t:m:cC:w:da:h" — a letter followed by ":" takes an argument).
//
// It supports clustered flags without arguments (-cd), an argument attached to
// its flag (-t240) or in the next token (-t 240), and stops option scanning at
// the first non-option token or at "--" (matching shell getopts). Everything
// after that point is returned as rest.
func getopts(argv []string, optspec string) (opts []opt, rest []string, err error) {
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
