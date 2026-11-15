package alert

import (
	"context"
	"errors"
	"testing"
)

type fake struct {
	calls int
	err   error
}

func (f *fake) Send(context.Context, Alert) error { f.calls++; return f.err }

func TestMulti(t *testing.T) {
	e1, e2 := errors.New("one"), errors.New("two")
	a, b, c := &fake{err: e1}, &fake{}, &fake{err: e2}
	err := Multi{a, b, c}.Send(context.Background(), Alert{})
	if a.calls != 1 || b.calls != 1 || c.calls != 1 {
		t.Fatalf("calls = %d %d %d", a.calls, b.calls, c.calls)
	}
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("err = %v", err)
	}
	if err := (Multi{b}).Send(context.Background(), Alert{}); err != nil {
		t.Fatal(err)
	}
}
