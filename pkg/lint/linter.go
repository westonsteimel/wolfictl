package lint

import (
	"fmt"
	"log"

	"golang.org/x/exp/slices"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/wolfi-dev/wolfictl/pkg/melange"
)

// Linter represents a linter instance.
type Linter struct {
	// options are the options to configure the linter.
	options Options

	// logger is the logger to use.
	logger *log.Logger
}

// New initializes a new instance of Linter.
func New(opts ...Option) *Linter {
	o := Options{}
	for _, opt := range opts {
		opt(&o)
	}
	return &Linter{
		options: o,
		logger:  log.New(log.Writer(), "", log.LstdFlags|log.Lmsgprefix),
	}
}

// Lint evaluates all rules and returns the result.
func (l *Linter) Lint() (Result, error) {
	rules := AllRules(l)

	filesToLint, err := melange.ReadAllPackagesFromRepo(l.options.Path)
	if err != nil {
		return Result{}, err
	}

	results := make(Result, 0)
	for name := range filesToLint {
		failedRules := make(EvalRuleErrors, 0)
		for _, rule := range rules {
			// Check if we should skip this rule.
			shouldEvaluate := true
			if len(rule.ConditionFuncs) > 0 {
				for _, cond := range rule.ConditionFuncs {
					if !cond() {
						shouldEvaluate = false
						break
					}
				}
			}

			// If one of the conditions is not met we skip the evaluation process.
			if !shouldEvaluate {
				if l.options.Verbose {
					l.logger.Printf("%s: skipping rule %s because condition is not met\n", name, rule.Name)
				}
				continue
			}

			// Allow users to override rules when running lint command
			if slices.Contains(l.options.SkipRules, rule.Name) {
				if l.options.Verbose {
					l.logger.Printf("%s: skipping rule %s because --skip-rule flag set\n", name, rule.Name)
				}
				continue
			}

			if slices.Contains(filesToLint[name].NoLint, rule.Name) {
				if l.options.Verbose {
					l.logger.Printf("%s: skipping rule %s because file contains #nolint:%s\n", name, rule.Name, rule.Name)
				}
				continue
			}

			// Evaluate the rule.
			if err := rule.LintFunc(filesToLint[name].Config); err != nil {
				msg := fmt.Sprintf("[%s]: %s (%s)", rule.Name, err.Error(), rule.Severity)
				if l.options.Verbose {
					msg += fmt.Sprintf(" - (%s)", rule.Description)
				}

				failedRules = append(failedRules, EvalRuleError{
					Rule:  rule,
					Error: fmt.Errorf(msg),
				})
			}
		}
		// If we have errors we append them to the result.
		if failedRules.WrapErrors() != nil {
			results = append(results, EvalResult{
				File:   name,
				Errors: failedRules,
			})
		}
	}

	return results, nil
}

// Print prints the result to stdout.
func (l *Linter) Print(result Result) {
	foundAny := false
	for _, res := range result {
		if res.Errors.WrapErrors() != nil {
			foundAny = true
			l.logger.Printf("Package: %s: %s\n", res.File, res.Errors.WrapErrors())
		}
	}
	if !foundAny {
		l.logger.Println("No linting issues found!")
	}
}

// PrintRules prints the rules to stdout.
func (l *Linter) PrintRules() {
	l.logger.Println("Available rules:")
	for _, rule := range AllRules(l) {
		l.logger.Printf("* %s: %s\n", rule.Name, cases.Title(language.Und).String(rule.Description))
	}
}
