package textru

import (
	"testing"
	"time"
)

func TestPlural(t *testing.T) {
	cases := map[int64]string{1: "день", 2: "дня", 4: "дня", 5: "дней", 11: "дней", 14: "дней", 21: "день", 22: "дня", 111: "дней", 0: "дней"}
	for n, want := range cases {
		if got := Plural(n, "день", "дня", "дней"); got != want {
			t.Errorf("Plural(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{40 * time.Second, "40 с"},
		{95 * time.Minute, "1 ч 35 мин"},
		{2*time.Hour + 5*time.Second, "2 ч"},
		{26*time.Hour + 30*time.Minute, "1 дн 2 ч"},
		{3*time.Minute + 2*time.Second, "3 мин 2 с"},
		{200 * time.Millisecond, "меньше секунды"},
	}
	for _, c := range cases {
		if got := Duration(c.d); got != c.want {
			t.Errorf("Duration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestPercentNeverRoundsUpToPerfect(t *testing.T) {
	cases := map[float64]string{1: "100%", 0.99996: "99,99%", 0.9512: "95,12%", 0: "0,00%"}
	for in, want := range cases {
		if got := Percent(in); got != want {
			t.Errorf("Percent(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDate(t *testing.T) {
	d := time.Date(2026, time.May, 7, 14, 5, 0, 0, time.UTC)
	if got := DateTime(d); got != "7 мая, 14:05" {
		t.Errorf("DateTime = %q", got)
	}
}
