package meter

import "testing"

func TestCeilingCounterSeedSetsRunningTotal(t *testing.T) {
	c := newCeilingCounter(100)
	c.add(10)
	c.seed(95)
	if c.exceeded() {
		t.Fatal("exceeded() = true at total 95 of 100, want false")
	}
	c.add(5)
	if !c.exceeded() {
		t.Fatal("exceeded() = false after seeding 95 and adding 5 against limit 100, want true")
	}
}

func TestCeilingCounterSeedReplacesInsteadOfAdding(t *testing.T) {
	c := newCeilingCounter(100)
	c.add(90)
	c.seed(20)
	if c.exceeded() {
		t.Fatal("exceeded() = true after reseeding to 20, want false (seed replaces the total)")
	}
	if c.total != 20 {
		t.Fatalf("total = %d, want 20", c.total)
	}
}

func TestCeilingCounterSeedAtLimitIsExceededAndNegativeIsZero(t *testing.T) {
	c := newCeilingCounter(100)
	c.seed(100)
	if !c.exceeded() {
		t.Fatal("exceeded() = false after seeding the limit, want true")
	}
	c = newCeilingCounter(100)
	c.seed(-5)
	if c.total != 0 || c.exceeded() {
		t.Fatalf("total = %d exceeded = %v after seeding -5, want 0 and false", c.total, c.exceeded())
	}
}

func TestCeilingCounterSeedKeepsForcedExceeded(t *testing.T) {
	c := newCeilingCounter(100)
	c.forceExceed()
	c.seed(0)
	if !c.exceeded() {
		t.Fatal("exceeded() = false after seed(0) on a forced counter, want true (seed never clears forceExceeded)")
	}
}
