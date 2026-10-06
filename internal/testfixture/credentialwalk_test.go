package testfixture

import (
	"reflect"
	"testing"
)

// These are FindCredentialLeak's own self-tests: proof the detector every
// caller (internal/workflow, internal/request, cmd/factoryd) relies on
// actually catches what it claims to, and that its exemptPaths mechanism
// exempts only an exact path with a matching Kind rather than silently
// widening. A caller's own test only needs to assert its real type is
// clean — this file is where "clean" is proven meaningful.

func TestFindCredentialLeakCatchesForbiddenType(t *testing.T) {
	type Credential struct{ Value string }
	type Carrier struct{ C Credential }
	forbidden := map[reflect.Type]bool{reflect.TypeOf(Credential{}): true}
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), forbidden, nil); err == nil {
		t.Fatal("expected an error for a field of a forbidden type, got nil")
	}
}

func TestFindCredentialLeakCatchesNameMatch(t *testing.T) {
	type Carrier struct{ APIKey string }
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, nil); err == nil {
		t.Fatal("expected an error for a credential-named field, got nil")
	}
}

func TestFindCredentialLeakExactPathExemptionSkipsAMatchingKind(t *testing.T) {
	type Carrier struct{ AllowNoCredential bool }
	exempt := map[string]reflect.Kind{"Carrier.AllowNoCredential": reflect.Bool}
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, exempt); err != nil {
		t.Fatalf("exact-path exemption with a matching kind should have skipped this field, got: %v", err)
	}
}

func TestFindCredentialLeakNonMatchingPathIsClean(t *testing.T) {
	type Carrier struct{ Name string }
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, nil); err != nil {
		t.Fatalf("a plain, non-credential-shaped struct must not be flagged, got: %v", err)
	}
	// An exemption for an unrelated path must not accidentally mask a real
	// violation elsewhere in the same struct.
	type CarrierWithSecret struct {
		Name   string
		Secret string
	}
	exempt := map[string]reflect.Kind{"CarrierWithSecret.Name": reflect.String}
	if err := FindCredentialLeak(reflect.TypeOf(CarrierWithSecret{}), nil, exempt); err == nil {
		t.Fatal("an exemption for Name must not exempt the unrelated Secret field, got nil")
	}
}

func TestFindCredentialLeakKindMismatchStillFails(t *testing.T) {
	type Carrier struct{ AllowNoCredential bool }
	// Exemption recorded against the WRONG kind: a later type change away
	// from bool must re-trip the guard, not stay silently exempted.
	exempt := map[string]reflect.Kind{"Carrier.AllowNoCredential": reflect.String}
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, exempt); err == nil {
		t.Fatal("expected a kind mismatch to fail the exemption, got nil")
	}
}

func TestFindCredentialLeakReportsInterfaceFields(t *testing.T) {
	type Carrier struct{ Usage map[string]any }
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, nil); err == nil {
		t.Fatal("expected an interface-kind map element to be reported, got nil")
	}

	exempt := map[string]reflect.Kind{"Carrier.Usage[]": reflect.Interface}
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, exempt); err != nil {
		t.Fatalf("an explicitly allow-listed interface path should have been skipped, got: %v", err)
	}
	// A kind mismatch on an interface exemption (e.g. a stale entry left
	// after the field's type changed) must still fail.
	staleExempt := map[string]reflect.Kind{"Carrier.Usage[]": reflect.String}
	if err := FindCredentialLeak(reflect.TypeOf(Carrier{}), nil, staleExempt); err == nil {
		t.Fatal("expected a stale (mismatched-kind) interface exemption to still fail, got nil")
	}
}
