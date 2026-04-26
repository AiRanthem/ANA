package infraops

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFactorySetRegisterAndGet(t *testing.T) {
	t.Parallel()

	set := NewFactorySet()
	wantFactory := func(context.Context, Options) (InfraOps, error) {
		return nil, nil
	}

	if err := set.Register(InfraType("localdir"), wantFactory); err != nil {
		t.Fatalf("register factory: %v", err)
	}

	gotFactory, ok := set.Get(InfraType("localdir"))
	if !ok {
		t.Fatalf("expected registered factory")
	}

	if reflect.ValueOf(gotFactory).Pointer() != reflect.ValueOf(wantFactory).Pointer() {
		t.Fatalf("factory pointer mismatch")
	}
}

func TestFactorySetRegisterRejectsDuplicateType(t *testing.T) {
	t.Parallel()

	set := NewFactorySet()
	first := func(context.Context, Options) (InfraOps, error) { return nil, nil }
	second := func(context.Context, Options) (InfraOps, error) { return nil, nil }

	if err := set.Register(InfraType("localdir"), first); err != nil {
		t.Fatalf("register first factory: %v", err)
	}

	err := set.Register(InfraType("localdir"), second)
	if !errors.Is(err, ErrInfraTypeConflict) {
		t.Fatalf("expected ErrInfraTypeConflict, got %v", err)
	}
}

func TestFactorySetTypesReturnsSortedSnapshot(t *testing.T) {
	t.Parallel()

	set := NewFactorySet()
	f := func(context.Context, Options) (InfraOps, error) { return nil, nil }

	if err := set.Register(InfraType("zeta"), f); err != nil {
		t.Fatalf("register zeta: %v", err)
	}
	if err := set.Register(InfraType("alpha"), f); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := set.Register(InfraType("beta"), f); err != nil {
		t.Fatalf("register beta: %v", err)
	}

	got := set.Types()
	want := []InfraType{"alpha", "beta", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("types mismatch, got %v want %v", got, want)
	}

	got[0] = "mutated"

	gotAgain := set.Types()
	if !reflect.DeepEqual(gotAgain, want) {
		t.Fatalf("types must be snapshot copy, got %v want %v", gotAgain, want)
	}
}
