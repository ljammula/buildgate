package composeservices

import (
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// TestServiceConfigFieldsAreFullyClassified is this package's replacement
// for the old hand-walker's core invariant ("every field present is either
// allow-listed or explicitly rejected, nothing silently dropped"), now
// that the fields live on compose-go's own types.ServiceConfig instead of
// a raw yaml.Node map this package owned outright. It reflects over the
// live imported type -- not a hardcoded list of field names copied once --
// so a go.mod bump to compose-go that adds a new field fails this test
// loudly instead of the new field silently passing through unvalidated.
func TestServiceConfigFieldsAreFullyClassified(t *testing.T) {
	typ := reflect.TypeOf(types.ServiceConfig{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		_, allowed := allowedFields[name]
		_, rejected := rejectedFields[name]
		switch {
		case allowed && rejected:
			t.Errorf("field %q is in both allowedFields and rejectedFields", name)
		case !allowed && !rejected:
			t.Errorf("field %q is unclassified -- add it to allowedFields or rejectedFields in parse.go", name)
		}
	}
}
