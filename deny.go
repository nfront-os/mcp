package main

// The deny list (-deny file): commands and paths the server refuses. It
// keeps an agent from running what it should not by accident; it is no
// sandbox: rc can spell any command so that no pattern sees it (variables,
// eval, a script). The disks' protection is diskGuard's driver mask.
//
// One rule a line, Go's regular expressions (RE2); # starts a comment line.
//	REGEX         refuse an rc command line the expression matches
//	path:REGEX    refuse write and edit of a path it matches (the cleaned,
//	              absolute path)

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type denyRule struct {
	re   *regexp.Regexp
	src  string
	line int
	path bool
}

func loadDeny(file string) ([]denyRule, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rules []denyRule
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		r := denyRule{src: l, line: n}
		if p, ok := strings.CutPrefix(l, "path:"); ok {
			r.path, l = true, strings.TrimSpace(p)
		}
		if r.re, err = regexp.Compile(l); err != nil {
			return nil, fmt.Errorf("%s:%d: %v", file, n, err)
		}
		rules = append(rules, r)
	}
	return rules, sc.Err()
}

// denied returns the rule that refuses s, or nil. A command is matched as a
// whole and line by line, so that ^ and $ mean a line's start and end too.
func denied(rules []denyRule, s string, path bool) *denyRule {
	for i := range rules {
		r := &rules[i]
		if r.path != path {
			continue
		}
		if r.re.MatchString(s) {
			return r
		}
		if !path {
			for _, l := range strings.Split(s, "\n") {
				if r.re.MatchString(strings.TrimSpace(l)) {
					return r
				}
			}
		}
	}
	return nil
}

func (r *denyRule) Error() string {
	return fmt.Sprintf("refused by the server's deny list (rule %d: %s). Do not try to get around it; ask the user.", r.line, r.src)
}

// writable refuses write and edit of a path the deny list or the disk guard
// keeps from the agent.
func writable(cfg *Config, p string) error {
	for _, d := range cfg.DiskPaths {
		if p == d || strings.HasPrefix(p, d+"/") {
			return fmt.Errorf("%s is a disk device: the server keeps disks from its tools (it runs without -disks)", p)
		}
	}
	if r := denied(cfg.Deny, p, true); r != nil {
		return r
	}
	return nil
}
