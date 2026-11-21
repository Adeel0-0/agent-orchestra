package main

import (
	"fmt"
	"strconv"
	"strings"
)

// flagSpec describes one --flag. Kind is "string", "int", "float" or "bool".
type flagSpec struct {
	Name    string
	Kind    string
	Default string
	Choices []string
	Help    string
}

// parsed holds the result of parsing one subcommand's arguments.
type parsed struct {
	positional []string
	values     map[string]string
	bools      map[string]bool
}

func (p parsed) str(name string) string { return p.values[name] }

func (p parsed) num(name string) float64 {
	f, _ := strconv.ParseFloat(p.values[name], 64)
	return f
}

// usageError is reported like argparse: exit code 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// parseArgs accepts flags before, between or after positionals (as argparse does),
// "--name value" and "--name=value". After stopAfter positionals, everything else is
// positional too (argparse.REMAINDER for `run <script> args...`); -1 disables that.
func parseArgs(args []string, specs []flagSpec, stopAfter int) (parsed, error) {
	p := parsed{values: map[string]string{}, bools: map[string]bool{}}
	byName := map[string]flagSpec{}
	for _, s := range specs {
		byName[s.Name] = s
		if s.Kind != "bool" {
			p.values[s.Name] = s.Default
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if stopAfter >= 0 && len(p.positional) >= stopAfter {
			p.positional = append(p.positional, args[i:]...)
			break
		}
		if a == "--" {
			p.positional = append(p.positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") || len(a) == 2 {
			p.positional = append(p.positional, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		spec, ok := byName[name]
		if !ok {
			return p, usageError{fmt.Sprintf("unrecognized arguments: %s", a)}
		}
		if spec.Kind == "bool" {
			if hasValue {
				return p, usageError{fmt.Sprintf("argument --%s: ignored explicit argument '%s'", name, value)}
			}
			p.bools[name] = true
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return p, usageError{fmt.Sprintf("argument --%s: expected one argument", name)}
			}
			i++
			value = args[i]
		}
		if err := checkValue(spec, value); err != nil {
			return p, err
		}
		p.values[name] = value
	}
	return p, nil
}

func checkValue(spec flagSpec, value string) error {
	switch spec.Kind {
	case "int":
		if _, err := strconv.Atoi(value); err != nil {
			return usageError{fmt.Sprintf("argument --%s: invalid int value: '%s'", spec.Name, value)}
		}
	case "float":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return usageError{fmt.Sprintf("argument --%s: invalid float value: '%s'", spec.Name, value)}
		}
	}
	if len(spec.Choices) > 0 {
		for _, c := range spec.Choices {
			if c == value {
				return nil
			}
		}
		return usageError{fmt.Sprintf("argument --%s: invalid choice: '%s' (choose from %s)", spec.Name, value, quotedList(spec.Choices))}
	}
	return nil
}

// quotedList formats a list like Python's repr: ['a', 'b'].
func quotedList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
	}
	return strings.Join(parts, ", ")
}

func pyList(items []string) string { return "[" + quotedList(items) + "]" }
