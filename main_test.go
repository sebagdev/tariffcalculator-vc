package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestParseCSVHandlesPolishDecimalsAndHourEndingLabels(t *testing.T) {
	input := `Data; Strefa; Wartość kWh;Rodzaj;
2026-04-30 1:00;T1;0,095;pobór;
2026-04-30 24:00;T1;1,250;pobór;
`
	readings, err := parseCSV(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 {
		t.Fatalf("expected 2 readings, got %d", len(readings))
	}

	wantStart := time.Date(2026, 4, 30, 0, 0, 0, 0, time.Local)
	if !readings[0].Start.Equal(wantStart) {
		t.Fatalf("first start = %s, want %s", readings[0].Start, wantStart)
	}
	if readings[0].KWh != 0.095 {
		t.Fatalf("first kWh = %.3f, want 0.095", readings[0].KWh)
	}

	wantLast := time.Date(2026, 4, 30, 23, 0, 0, 0, time.Local)
	if !readings[1].Start.Equal(wantLast) {
		t.Fatalf("last start = %s, want %s", readings[1].Start, wantLast)
	}
}

func TestTariffWindows(t *testing.T) {
	fridayNoon := time.Date(2026, 5, 1, 12, 0, 0, 0, time.Local)
	fridayFourteen := time.Date(2026, 5, 1, 14, 0, 0, 0, time.Local)
	saturdayNoon := time.Date(2026, 5, 2, 12, 0, 0, 0, time.Local)
	winterAfternoon := time.Date(2026, 1, 5, 16, 0, 0, 0, time.Local)
	summerEvening := time.Date(2026, 7, 6, 19, 0, 0, 0, time.Local)

	if !g12Day(fridayNoon) || g12Day(fridayFourteen) {
		t.Fatal("G12 day window mismatch")
	}
	if !g12wPeak(fridayNoon) || g12wPeak(saturdayNoon) {
		t.Fatal("G12w weekend/peak window mismatch")
	}
	if !g13AfternoonPeak(winterAfternoon) || !g13AfternoonPeak(summerEvening) {
		t.Fatal("G13 seasonal afternoon peak mismatch")
	}
}

func TestProjectYearFallsBackToHourAverage(t *testing.T) {
	readings := []Reading{
		{Start: time.Date(2026, 5, 1, 8, 0, 0, 0, time.Local), KWh: 2},
		{Start: time.Date(2026, 5, 2, 8, 0, 0, 0, time.Local), KWh: 4},
	}
	projected := projectYear(readings, 2026)
	for _, reading := range projected {
		if reading.Start.Equal(time.Date(2026, 1, 1, 8, 0, 0, 0, time.Local)) {
			if reading.KWh != 3 {
				t.Fatalf("fallback hour average = %.2f, want 3.00", reading.KWh)
			}
			return
		}
	}
	t.Fatal("projected timestamp not found")
}

func TestPastedCSVAndWebTemplateRenderComparison(t *testing.T) {
	input := `Data; Strefa; Wartość kWh;Rodzaj;
2026-04-30 1:00;T1;0,095;pobór;
2026-04-30 2:00;T1;0,084;pobór;
Data; Strefa; Wartość kWh;Rodzaj;
2026-04-30 1:00;T1;0,095;pobór;
2026-04-30 3:00;T1;0,097;pobór;
`
	report, err := comparePastedCSV(compareForm{
		CSV:           input,
		Year:          2026,
		Phase:         3,
		PowerKW:       1,
		BillingMonths: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Input.Readings != 3 {
		t.Fatalf("unique readings = %d, want 3", report.Input.Readings)
	}
	if report.Input.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", report.Input.Duplicates)
	}

	tmpl, err := template.New("app").Funcs(templateFuncs()).Parse(appTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = tmpl.ExecuteTemplate(&out, "results", pageData{
		Form:   compareForm{CSV: input, Year: 2026, Phase: 3, PowerKW: 1, BillingMonths: 12},
		Report: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, "is cheapest for this profile") || !strings.Contains(html, "G13 zones") {
		t.Fatalf("rendered comparison missing expected content: %s", html)
	}
}

func TestPastedCSVAcceptsQuotedEscapedTextareaValue(t *testing.T) {
	input := `"Data; Strefa; Wartość kWh;Rodzaj;\n2026-04-30 1:00;T1;0,095;pobór;\n2026-04-30 2:00;T1;0,084;pobór;\n\nData; Strefa; Wartość kWh;Rodzaj;\n2026-04-30 1:00;T1;0,095;pobór;\n2026-04-30 3:00;T1;0,097;pobór;\n"`
	readings, skipped, err := parsePastedCSVs(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 3 {
		t.Fatalf("readings = %d, want 3", len(readings))
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
}
