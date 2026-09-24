package sessionruntime

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hiveryn/daemon/internal/domain"
)

// Approval inputs: a pending intent may carry a small schema of fields the user
// completes while approving (see domain.IntentInputField). The daemon is the
// only authority on the values — the desktop's checks are a convenience — and
// the operation's Exec receives only values that passed resolveIntentInputs.

// checkIntentInputSchema rejects a structurally broken schema. The schema is
// authored by a daemon tool, so a failure here is a programmer error returned
// to the caller before any popup, not something a user can correct. Defaults
// are deliberately NOT checked here: they only prefill the form and may come
// from mutable configuration, so a stale one costs the user a correction, not
// the request.
func checkIntentInputSchema(fields []domain.IntentInputField) error {
	if len(fields) > domain.MaxIntentInputFields {
		return fmt.Errorf("intent input schema has %d fields, limit is %d", len(fields), domain.MaxIntentInputFields)
	}
	seen := map[string]struct{}{}
	for i, f := range fields {
		if strings.TrimSpace(f.Name) == "" || f.Name != strings.TrimSpace(f.Name) {
			return fmt.Errorf("intent input %d: name %q must be non-empty without surrounding whitespace", i, f.Name)
		}
		if _, dup := seen[f.Name]; dup {
			return fmt.Errorf("intent input %q: duplicate name", f.Name)
		}
		seen[f.Name] = struct{}{}
		if strings.TrimSpace(f.Label) == "" {
			return fmt.Errorf("intent input %q: label is required", f.Name)
		}
		if !f.Type.Valid() {
			return fmt.Errorf("intent input %q: unsupported type %q", f.Name, f.Type)
		}
		if f.MaxLength < 0 {
			return fmt.Errorf("intent input %q: max_length must not be negative", f.Name)
		}
		if f.MaxLength > 0 && f.Type != domain.IntentInputText && f.Type != domain.IntentInputTextarea {
			return fmt.Errorf("intent input %q: max_length applies only to text and textarea", f.Name)
		}
		if f.Type != domain.IntentInputChoice {
			if len(f.Options) > 0 {
				return fmt.Errorf("intent input %q: options apply only to choice", f.Name)
			}
			continue
		}
		if len(f.Options) == 0 {
			return fmt.Errorf("intent input %q: choice needs at least one option", f.Name)
		}
		if len(f.Options) > domain.MaxIntentInputOptions {
			return fmt.Errorf("intent input %q: %d options, limit is %d", f.Name, len(f.Options), domain.MaxIntentInputOptions)
		}
		values := map[string]struct{}{}
		for _, o := range f.Options {
			if o.Value == "" {
				return fmt.Errorf("intent input %q: option values must be non-empty", f.Name)
			}
			if _, dup := values[o.Value]; dup {
				return fmt.Errorf("intent input %q: duplicate option %q", f.Name, o.Value)
			}
			values[o.Value] = struct{}{}
		}
	}
	return nil
}

// resolveIntentInputs validates submitted values against the schema and
// returns the values the operation runs with. Values are taken as submitted:
// a default never stands in for an absent one, so an intent is only ever run
// with what the user explicitly approved. Optional fields left empty are
// omitted from the result. Issues are sorted by field for stable messages.
func resolveIntentInputs(fields []domain.IntentInputField, submitted domain.IntentInputValues) (domain.IntentInputValues, []domain.IntentInputIssue) {
	var issues []domain.IntentInputIssue
	known := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		known[f.Name] = struct{}{}
	}
	for name := range submitted {
		if _, ok := known[name]; !ok {
			issues = append(issues, domain.IntentInputIssue{Field: name, Message: "is not an input of this intent"})
		}
	}

	var out domain.IntentInputValues
	for _, f := range fields {
		value, present, msg := checkIntentInputValue(f, submitted[f.Name])
		if msg != "" {
			issues = append(issues, domain.IntentInputIssue{Field: f.Name, Message: msg})
			continue
		}
		if !present {
			if f.Required {
				issues = append(issues, domain.IntentInputIssue{Field: f.Name, Message: "is required"})
			}
			continue
		}
		if out == nil {
			out = domain.IntentInputValues{}
		}
		out[f.Name] = value
	}

	if len(issues) > 0 {
		sort.SliceStable(issues, func(i, j int) bool { return issues[i].Field < issues[j].Field })
		return nil, issues
	}
	return out, nil
}

// checkIntentInputValue validates one value. present is false for an absent or
// empty value, which is an issue only when the field is required.
func checkIntentInputValue(f domain.IntentInputField, raw any) (value any, present bool, issue string) {
	if raw == nil {
		return nil, false, ""
	}
	switch f.Type {
	case domain.IntentInputBoolean:
		b, ok := raw.(bool)
		if !ok {
			return nil, false, fmt.Sprintf("must be a boolean, got %T", raw)
		}
		return b, true, ""
	case domain.IntentInputText, domain.IntentInputTextarea, domain.IntentInputChoice:
		s, ok := raw.(string)
		if !ok {
			return nil, false, fmt.Sprintf("must be a string, got %T", raw)
		}
		if strings.TrimSpace(s) == "" {
			return nil, false, ""
		}
		switch f.Type {
		case domain.IntentInputText:
			if strings.ContainsAny(s, "\r\n") {
				return nil, false, "must be a single line"
			}
		case domain.IntentInputChoice:
			for _, o := range f.Options {
				if o.Value == s {
					return s, true, ""
				}
			}
			return nil, false, fmt.Sprintf("%q is not one of the offered options", s)
		}
		if f.MaxLength > 0 {
			if n := utf8.RuneCountInString(s); n > f.MaxLength {
				return nil, false, fmt.Sprintf("is %d characters, limit is %d", n, f.MaxLength)
			}
		}
		return s, true, ""
	default:
		return nil, false, fmt.Sprintf("has unsupported type %q", f.Type)
	}
}

// intentInputsError turns submission issues into the ValidationError the
// approve route returns as a 400. The intent stays pending, so the user can
// correct the values and approve again.
func intentInputsError(issues []domain.IntentInputIssue) error {
	parts := make([]string, 0, len(issues))
	for _, is := range issues {
		parts = append(parts, is.Field+" "+is.Message)
	}
	return &domain.ValidationError{Field: "inputs", Message: "invalid: " + strings.Join(parts, "; ")}
}
