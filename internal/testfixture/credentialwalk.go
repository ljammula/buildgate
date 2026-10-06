package testfixture

import (
	"fmt"
	"reflect"
	"strings"
)

// CredentialFieldSubstrings are lower-cased field-name substrings that
// suggest a credential-shaped field. "Token" alone is deliberately absent:
// e.g. TokenBudget is a budget, not a credential. This is the single list
// every credential-reflection guard in the repo checks against, so they all
// agree on what counts as credential-shaped.
var CredentialFieldSubstrings = []string{"apikey", "secret", "password", "credential", "passphrase", "privatekey", "bearer"}

// FindCredentialLeak reflect-walks typ (a struct type, typically a durable
// record such as a Temporal workflow input, a request record, or a queue
// entry) and returns a descriptive error for the first field that:
//   - has a type present in forbiddenTypes (a type known to hold a raw
//     credential, e.g. sandbox.RouteSecret), or
//   - has a name containing one of CredentialFieldSubstrings, or
//   - has reflect.Interface kind (including a map's or slice's element
//     type): this walk cannot see through an interface to its concrete
//     runtime value, so an un-allow-listed interface-kind field is reported
//     on the fail-closed assumption that it could hold a credential.
//
// exemptPaths maps a field's full dotted path, exactly as this walk would
// name it in an error (e.g. "Run.SomeAllowCredentialFlag", or
// "Request.SpecEvidence[].Usage[]" for a map's element type), to the
// reflect.Kind that path's field is expected to have. A path only escapes
// its would-be violation when BOTH the path and its recorded Kind match —
// so a later, unrelated change to that field's type (e.g. a bool renamed
// into a string, or a map[string]any replaced by something narrower) re-
// trips this guard instead of leaving a now-stale exemption silently in
// place. Callers document, at the call site, why each exemption is safe.
//
// It returns nil if the walk finds nothing.
func FindCredentialLeak(typ reflect.Type, forbiddenTypes map[reflect.Type]bool, exemptPaths map[string]reflect.Kind) error {
	return findCredentialLeak(typ, typ.Name(), forbiddenTypes, exemptPaths, map[reflect.Type]bool{})
}

func findCredentialLeak(typ reflect.Type, path string, forbiddenTypes map[reflect.Type]bool, exemptPaths map[string]reflect.Kind, seen map[reflect.Type]bool) error {
	if forbiddenTypes[typ] {
		return fmt.Errorf("%s reaches credential-bearing type %s", path, typ)
	}
	if typ.Kind() == reflect.Interface {
		if kind, ok := exemptPaths[path]; ok && kind == reflect.Interface {
			return nil
		}
		return fmt.Errorf("%s is an interface-kind field (%s): its concrete value is unknown to this walk and could be a credential; allow-list it in exemptPaths (with reflect.Interface) if it is known-safe", path, typ)
	}
	if seen[typ] {
		return nil
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Map:
		if err := findCredentialLeak(typ.Key(), path+"[key]", forbiddenTypes, exemptPaths, seen); err != nil {
			return err
		}
		return findCredentialLeak(typ.Elem(), path+"[]", forbiddenTypes, exemptPaths, seen)
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findCredentialLeak(typ.Elem(), path+"[]", forbiddenTypes, exemptPaths, seen)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			fieldPath := path + "." + field.Name
			lowered := strings.ToLower(field.Name)
			nameMatched := false
			for _, forbidden := range CredentialFieldSubstrings {
				if strings.Contains(lowered, forbidden) {
					nameMatched = true
					break
				}
			}
			if nameMatched {
				if kind, ok := exemptPaths[fieldPath]; !ok || kind != field.Type.Kind() {
					return fmt.Errorf("%s is a credential-named field", fieldPath)
				}
				// Exempted: still recurse below, so an exempted container
				// field (there are none of these today, but nothing here
				// assumes it stays that way) still has its own contents
				// checked.
			}
			if err := findCredentialLeak(field.Type, fieldPath, forbiddenTypes, exemptPaths, seen); err != nil {
				return err
			}
		}
	}
	return nil
}
