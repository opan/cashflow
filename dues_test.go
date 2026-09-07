package main

import (
	"strings"
	"testing"
	"time"
)

func d(y int, m time.Month, day int) time.Time {
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

func TestPeriodIndex(t *testing.T) {
	start := d(2026, 9, 1)
	cases := []struct {
		period string
		when   time.Time
		want   int
	}{
		{"monthly", d(2026, 9, 1), 0},
		{"monthly", d(2026, 9, 30), 0},
		{"monthly", d(2026, 11, 15), 2},
		{"monthly", d(2026, 8, 1), 0}, // before start clamps to 0
		{"bimonthly", d(2026, 11, 1), 1},
		{"quarterly", d(2027, 3, 1), 2},
		{"yearly", d(2029, 1, 1), 2},
		{"weekly", d(2026, 9, 15), 2},
	}
	for _, c := range cases {
		if got := periodIndex(start, c.period, c.when); got != c.want {
			t.Errorf("periodIndex(%s, %s) = %d, want %d", c.period, c.when.Format("2006-01-02"), got, c.want)
		}
	}
}

func TestPeriodLabel(t *testing.T) {
	start := d(2026, 9, 1)
	cases := []struct {
		period string
		i      int
		want   string
	}{
		{"monthly", 0, "September 2026"},
		{"monthly", 2, "November 2026"},
		{"bimonthly", 0, "Sep–Okt 2026"},
		{"quarterly", 0, "Sep–Nov 2026"},
		{"yearly", 1, "2027"},
		{"weekly", 1, "8 Sep"},
	}
	for _, c := range cases {
		if got := periodLabel(start, c.period, c.i); got != c.want {
			t.Errorf("periodLabel(%s, %d) = %q, want %q", c.period, c.i, got, c.want)
		}
	}
}

// TestBuildDuesTable_Waterfall is the user's example: 30k monthly dues, a member
// who paid 70k covers 2 full months plus a partial third (short 20k), and a
// member who paid nothing shows all "belum".
func TestBuildDuesTable_Waterfall(t *testing.T) {
	plan := &CashPlan{DueAmount: 30000, DuePeriod: "monthly", DueStart: d(2026, 9, 1)}
	members := []Member{{ID: "1", Name: "Andi"}, {ID: "2", Name: "Budi"}}
	paid := map[string]int64{"andi": 70000} // Budi paid nothing
	now := d(2026, 11, 10)

	tbl := buildDuesTable(plan, members, paid, now)

	if len(tbl.Columns) != 3 {
		t.Fatalf("columns = %d, want 3 (%v)", len(tbl.Columns), tbl.Columns)
	}
	andi := tbl.Rows[0]
	if andi.Cells[0].Status != "lunas" || andi.Cells[1].Status != "lunas" {
		t.Errorf("Andi months 1-2 should be lunas, got %q,%q", andi.Cells[0].Status, andi.Cells[1].Status)
	}
	if andi.Cells[2].Status != "sebagian" || andi.Cells[2].Paid != 10000 || andi.Cells[2].Short != 20000 {
		t.Errorf("Andi month 3 = %+v, want sebagian paid=10000 short=20000", andi.Cells[2])
	}
	if !strings.Contains(andi.StatusText, "Lunas s/d Oktober 2026") || !strings.Contains(andi.StatusText, "kurang Rp 20.000 untuk November 2026") {
		t.Errorf("Andi status = %q", andi.StatusText)
	}

	budi := tbl.Rows[1]
	for i, c := range budi.Cells {
		if c.Status != "belum" {
			t.Errorf("Budi cell %d = %q, want belum", i, c.Status)
		}
	}
	if budi.StatusText != "Belum bayar" {
		t.Errorf("Budi status = %q, want 'Belum bayar'", budi.StatusText)
	}
}

// TestBuildDuesTable_Prepaid: paying ahead extends the table beyond today.
func TestBuildDuesTable_Prepaid(t *testing.T) {
	plan := &CashPlan{DueAmount: 30000, DuePeriod: "monthly", DueStart: d(2026, 9, 1)}
	members := []Member{{ID: "1", Name: "Andi"}}
	paid := map[string]int64{"andi": 120000} // 4 full months
	now := d(2026, 9, 5)                     // only period 0 so far

	tbl := buildDuesTable(plan, members, paid, now)
	if len(tbl.Columns) != 4 {
		t.Fatalf("columns = %d, want 4 (prepaid should extend)", len(tbl.Columns))
	}
	for i := 0; i < 4; i++ {
		if tbl.Rows[0].Cells[i].Status != "lunas" {
			t.Errorf("prepaid cell %d = %q, want lunas", i, tbl.Rows[0].Cells[i].Status)
		}
	}
}
