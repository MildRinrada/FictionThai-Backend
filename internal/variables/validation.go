package variables

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Field bounds. Counted in RUNES: a byte limit would give a Thai writer roughly
// a third of the room it gives an English one.
const (
	TokenMaxLength   = 32
	LabelMaxLength   = 64
	ValueMaxLength   = 64
	MaxPerNovel      = 20
	MaxChoiceOptions = 12
	MaxPronounForms  = 6
	MaxPronounSets   = 12
)

// DefaultToken is adopted when a writer adds a variable without naming one. It
// is the convention Thai reader-insert fiction already uses.
const DefaultToken = "(y/n)"

type validationErrors map[string][]string

func (v validationErrors) add(field, message string) {
	v[field] = append(v[field], message)
}

func (v validationErrors) err() error {
	if len(v) == 0 {
		return nil
	}
	return apierror.Validation(map[string][]string(v))
}

// safeText rejects anything that is not plain, human-readable content - the
// same floor chapter content is held to (docs/11 §17).
func safeText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}

func runeLen(value string) int { return utf8.RuneCountInString(value) }

// Input is one variable as submitted by a writer.
//
// There is deliberately no Position field: the server assigns positions from
// array order, so a gap or a duplicate is not representable - the same rule chat
// messages and headcanon entries follow (docs/CONTENT-MODEL.md §4).
type Input struct {
	Token        string   `json:"token"`
	Label        string   `json:"label"`
	DefaultValue *string  `json:"default_value"`
	Kind         string   `json:"kind"`
	Options      *Options `json:"options"`
}

// validateToken checks a placeholder.
//
// The token is matched LITERALLY at render - never compiled as a regular
// expression - so these rules are about it being a usable marker, not about
// pattern safety. Whitespace is refused because a token has to survive being
// typed inside a sentence and found again by an exact string search.
func validateToken(errs validationErrors, field, raw string) string {
	trimmed := strings.TrimSpace(raw)

	switch {
	case trimmed == "":
		errs.add(field, "A placeholder cannot be empty.")
	case runeLen(trimmed) > TokenMaxLength:
		errs.add(field, fmt.Sprintf(
			"A placeholder may be at most %d characters.", TokenMaxLength))
	case strings.ContainsFunc(trimmed, unicode.IsSpace):
		errs.add(field, "A placeholder cannot contain spaces.")
	case !safeText(trimmed):
		errs.add(field, "A placeholder must be plain text.")
	}
	return trimmed
}

func validateLine(errs validationErrors, field, value string, max int, required bool) string {
	trimmed := strings.TrimSpace(value)
	switch {
	case trimmed == "":
		if required {
			errs.add(field, "This cannot be empty.")
		}
	case runeLen(trimmed) > max:
		errs.add(field, fmt.Sprintf("Must be at most %d characters.", max))
	case !safeText(trimmed) || strings.ContainsAny(trimmed, "\n\r"):
		errs.add(field, "Contains characters that are not allowed.")
	}
	return trimmed
}

// Validate converts submitted variables into storable ones, assigning positions
// from array order.
//
// Duplicate TOKENS are rejected rather than silently deduplicated: two rows
// answering to one placeholder would make which answer wins depend on row order,
// and the database's UNIQUE would reject the write anyway - with an error naming
// a constraint instead of a field.
func Validate(inputs []Input) ([]Variable, error) {
	errs := validationErrors{}

	if len(inputs) > MaxPerNovel {
		errs.add("variables", fmt.Sprintf(
			"A fiction may declare at most %d variables.", MaxPerNovel))
		return nil, errs.err()
	}

	seen := map[string]int{}
	out := make([]Variable, 0, len(inputs))

	for i, input := range inputs {
		field := fmt.Sprintf("variables[%d]", i)

		token := validateToken(errs, field+".token", input.Token)
		label := validateLine(errs, field+".label", input.Label, LabelMaxLength, true)

		if token != "" {
			if first, duplicate := seen[token]; duplicate {
				errs.add(field+".token", fmt.Sprintf(
					"This placeholder is already used by variable %d.", first+1))
			} else {
				seen[token] = i
			}
		}

		kind := Kind(strings.TrimSpace(input.Kind))
		if kind == "" {
			kind = KindText
		}
		if !kind.Valid() {
			errs.add(field+".kind", fmt.Sprintf("Must be one of: %s.", joinKinds()))
			continue
		}

		var defaultValue *string
		if input.DefaultValue != nil {
			cleaned := validateLine(errs, field+".default_value", *input.DefaultValue,
				ValueMaxLength, false)
			if cleaned != "" {
				defaultValue = &cleaned
			}
		}

		options := validateOptions(errs, field, kind, input.Options)

		out = append(out, Variable{
			Position:     i,
			Token:        token,
			Label:        label,
			DefaultValue: defaultValue,
			Kind:         kind,
			Options:      options,
		})
	}

	if err := errs.err(); err != nil {
		return nil, err
	}
	return out, nil
}

// validateOptions checks the kind-specific configuration and DROPS whatever
// belongs to another kind.
//
// Dropping rather than rejecting is deliberate: a writer switching a variable
// from choice to text should not have to clear the old options by hand, and
// keeping them would leave a row whose stored shape contradicts its kind.
func validateOptions(errs validationErrors, field string, kind Kind, options *Options) *Options {
	switch kind {
	case KindChoice:
		values := []string{}
		if options != nil {
			if len(options.Values) > MaxChoiceOptions {
				errs.add(field+".options", fmt.Sprintf(
					"At most %d choices.", MaxChoiceOptions))
				return nil
			}
			for i, value := range options.Values {
				cleaned := validateLine(errs,
					fmt.Sprintf("%s.options.values[%d]", field, i), value, ValueMaxLength, true)
				if cleaned != "" {
					values = append(values, cleaned)
				}
			}
		}
		if len(values) == 0 {
			errs.add(field+".options", "A choice variable needs at least one option.")
			return nil
		}
		return &Options{Values: values}

	case KindPronoun:
		if options == nil || len(options.Forms) == 0 {
			errs.add(field+".options", "A pronoun variable needs at least one form.")
			return nil
		}
		if len(options.Forms) > MaxPronounForms {
			errs.add(field+".options", fmt.Sprintf("At most %d forms.", MaxPronounForms))
			return nil
		}
		if len(options.Sets) > MaxPronounSets {
			errs.add(field+".options", fmt.Sprintf("At most %d sets.", MaxPronounSets))
			return nil
		}

		forms := make([]string, 0, len(options.Forms))
		for i, form := range options.Forms {
			// A form name becomes part of a TOKEN, so it obeys the token rules
			// rather than the label rules: no spaces, or the placeholder it
			// builds could not be found again by an exact search.
			cleaned := validateToken(errs,
				fmt.Sprintf("%s.options.forms[%d]", field, i), form)
			if cleaned != "" {
				forms = append(forms, cleaned)
			}
		}

		sets := make([]PronounSet, 0, len(options.Sets))
		for i, set := range options.Sets {
			setField := fmt.Sprintf("%s.options.sets[%d]", field, i)
			label := validateLine(errs, setField+".label", set.Label, LabelMaxLength, true)

			values := make([]string, 0, len(forms))
			for j := range forms {
				value := ""
				if j < len(set.Values) {
					value = set.Values[j]
				}
				// Every form gets an answer, blank if the writer left it so -
				// a short row must not shift the remaining words onto the wrong
				// form.
				values = append(values, validateLine(errs,
					fmt.Sprintf("%s.values[%d]", setField, j), value, ValueMaxLength, false))
			}
			sets = append(sets, PronounSet{Label: label, Values: values})
		}
		if len(sets) == 0 {
			errs.add(field+".options", "A pronoun variable needs at least one set.")
			return nil
		}
		return &Options{Forms: forms, Sets: sets}

	default:
		// Text carries no options at all.
		return nil
	}
}

func joinKinds() string {
	out := make([]string, 0, len(Kinds()))
	for _, kind := range Kinds() {
		out = append(out, kind.String())
	}
	return strings.Join(out, ", ")
}
