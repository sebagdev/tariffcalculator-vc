package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	vatRate       = 0.23
	qualityFeeNet = 0.0331
	ozeFeeNet     = 0.00730
	cogenFeeNet   = 0.00030
	capacityNet   = 0.2194
)

type Reading struct {
	Start time.Time
	KWh   float64
}

type Tariff struct {
	Name  string
	Zones []Zone
	Fixed FixedFees
}

type Zone struct {
	Name            string
	EnergyGross     float64
	DistributionNet float64
	Match           func(time.Time) bool
}

type FixedFees struct {
	DistributionFixedNetPerKWMonth   float64
	DistributionSubscriptionNetMonth float64
	SellerSubscriptionGrossMonth     float64
}

type ZoneUsage struct {
	KWh       float64
	NetCost   float64
	GrossCost float64
}

type TariffResult struct {
	Name                    string
	TotalKWh                float64
	VariableNet             float64
	DistributionFixedNet    float64
	SellerSubscriptionGross float64
	NetTotal                float64
	GrossTotal              float64
	Zones                   map[string]ZoneUsage
}

type profileBucket struct {
	sum   float64
	count int
}

func main() {
	serve := flag.Bool("serve", true, "run the HTMX web application")
	cli := flag.Bool("cli", false, "run once in command-line mode instead of starting the web app")
	addr := flag.String("addr", "127.0.0.1:8080", "web server address")
	dataDir := flag.String("data", "../docs", "directory containing electricity CSV exports")
	year := flag.Int("year", time.Now().Year(), "calendar year to project")
	powerKW := flag.Float64("power-kw", 1.0, "contracted power in kW for fixed distribution charges")
	phase := flag.Int("phase", 3, "installation phase count: 1 or 3")
	billingMonths := flag.Int("billing-months", 12, "seller billing cycle in months: 1, 2, 6, or 12")
	showHourly := flag.Bool("show-hourly-profile", false, "print projected average kWh by weekday and hour")
	flag.Parse()

	if *phase != 1 && *phase != 3 {
		exitErr(fmt.Errorf("--phase must be 1 or 3"))
	}
	if _, ok := sellerSubscriptionGrossByBillingCycle(*billingMonths); !ok {
		exitErr(fmt.Errorf("--billing-months must be 1, 2, 6, or 12"))
	}
	if *serve && !*cli {
		if err := serveWeb(*addr, *dataDir, *year, *phase, *powerKW, *billingMonths); err != nil {
			exitErr(err)
		}
		return
	}

	readings, skipped, err := loadReadings(*dataDir)
	if err != nil {
		exitErr(err)
	}
	if len(readings) == 0 {
		exitErr(fmt.Errorf("no usage readings found in %s", *dataDir))
	}

	projection := projectYear(readings, *year)
	tariffs := buildTariffs(*phase, *billingMonths)
	results := make([]TariffResult, 0, len(tariffs))
	for _, tariff := range tariffs {
		results = append(results, calculateTariff(tariff, projection, *powerKW))
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].GrossTotal < results[j].GrossTotal
	})

	printInputSummary(readings, projection, *year, skipped)
	printResults(results)
	if *showHourly {
		printHourlyProfile(readings)
	}
}

type pageData struct {
	Form        compareForm
	Report      *comparisonReport
	SampleCSV   string
	Error       string
	CurrentYear int
}

type compareForm struct {
	CSV           string
	Year          int
	Phase         int
	PowerKW       float64
	BillingMonths int
}

type comparisonReport struct {
	Input           inputSummary
	Results         []TariffResult
	Best            TariffResult
	SavingsVsWorst  float64
	WeekdayProfiles []weekdayProfile
	HourProfiles    []hourProfile
	Assumptions     []string
}

type inputSummary struct {
	Readings       int
	Duplicates     int
	ObservedKWh    float64
	ProjectedKWh   float64
	ProjectedYear  int
	ProjectedHours int
	RangeStart     string
	RangeEnd       string
}

type weekdayProfile struct {
	Name string
	KWh  float64
}

type hourProfile struct {
	Hour string
	KWh  float64
}

func serveWeb(addr string, dataDir string, year int, phase int, powerKW float64, billingMonths int) error {
	tmpl, err := template.New("app").Funcs(templateFuncs()).Parse(appTemplate)
	if err != nil {
		return err
	}

	sample, _ := sampleCSV(dataDir)
	defaultForm := compareForm{
		CSV:           sample,
		Year:          year,
		Phase:         phase,
		PowerKW:       powerKW,
		BillingMonths: billingMonths,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data := pageData{Form: defaultForm, SampleCSV: sample, CurrentYear: time.Now().Year()}
		renderTemplate(w, tmpl, "page", data)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/compare", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		form, err := formFromRequest(r)
		data := pageData{Form: form, SampleCSV: sample, CurrentYear: time.Now().Year()}
		if err == nil {
			data.Report, err = comparePastedCSV(form)
		}
		if err != nil {
			data.Error = err.Error()
		}
		if r.Header.Get("HX-Request") == "true" {
			renderTemplate(w, tmpl, "results", data)
			return
		}
		renderTemplate(w, tmpl, "page", data)
	})

	fmt.Printf("Tariff web app listening on http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"money":      formatMoney,
		"kwh":        formatKWh,
		"pct":        formatPercent,
		"zoneNames":  sortedZoneNames,
		"zoneShare":  zoneShare,
		"minus":      func(a, b float64) float64 { return a - b },
		"mul":        func(a, b float64) float64 { return a * b },
		"rank":       func(i int) int { return i + 1 },
		"barWidth":   barWidth,
		"jsString":   jsString,
		"lower":      strings.ToLower,
		"htmlSafeID": htmlSafeID,
	}
}

func renderTemplate(w http.ResponseWriter, tmpl *template.Template, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func formFromRequest(r *http.Request) (compareForm, error) {
	year, err := strconv.Atoi(r.FormValue("year"))
	if err != nil {
		return compareForm{}, fmt.Errorf("year must be a number")
	}
	phase, err := strconv.Atoi(r.FormValue("phase"))
	if err != nil || (phase != 1 && phase != 3) {
		return compareForm{}, fmt.Errorf("phase must be 1 or 3")
	}
	powerKW, err := strconv.ParseFloat(strings.ReplaceAll(r.FormValue("powerKW"), ",", "."), 64)
	if err != nil || powerKW < 0 {
		return compareForm{}, fmt.Errorf("contracted power must be a non-negative number")
	}
	billingMonths, err := strconv.Atoi(r.FormValue("billingMonths"))
	if err != nil {
		return compareForm{}, fmt.Errorf("billing cycle must be a number")
	}
	if _, ok := sellerSubscriptionGrossByBillingCycle(billingMonths); !ok {
		return compareForm{}, fmt.Errorf("billing cycle must be 1, 2, 6, or 12 months")
	}
	return compareForm{
		CSV:           r.FormValue("csv"),
		Year:          year,
		Phase:         phase,
		PowerKW:       powerKW,
		BillingMonths: billingMonths,
	}, nil
}

func comparePastedCSV(form compareForm) (*comparisonReport, error) {
	readings, skipped, err := parsePastedCSVs(form.CSV)
	if err != nil {
		return nil, err
	}
	if len(readings) == 0 {
		return nil, fmt.Errorf("paste at least one CSV export with hourly pobór rows")
	}
	projection := projectYear(readings, form.Year)
	tariffs := buildTariffs(form.Phase, form.BillingMonths)
	results := make([]TariffResult, 0, len(tariffs))
	for _, tariff := range tariffs {
		results = append(results, calculateTariff(tariff, projection, form.PowerKW))
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].GrossTotal < results[j].GrossTotal
	})
	input := buildInputSummary(readings, projection, form.Year, skipped)
	return &comparisonReport{
		Input:           input,
		Results:         results,
		Best:            results[0],
		SavingsVsWorst:  results[len(results)-1].GrossTotal - results[0].GrossTotal,
		WeekdayProfiles: weekdayProfiles(readings),
		HourProfiles:    hourProfiles(readings),
		Assumptions: []string{
			"G12 day: 06:00-13:00 and 15:00-22:00; G12w makes weekends off-peak.",
			"G13 afternoon peak is seasonal: 16:00-21:00 in Oct-Mar, 19:00-22:00 in Apr-Sep.",
			"CSV labels 1:00..24:00 are treated as hour-ending labels, so 1:00 maps to 00:00-01:00.",
			"Projection learns weekday/hour averages and falls back to hour averages when a weekday/hour is missing.",
		},
	}, nil
}

func sampleCSV(dataDir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dataDir, "*.csv"))
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	var out strings.Builder
	for i, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		if i > 0 {
			out.WriteString("\n")
		}
		out.Write(b)
	}
	return out.String(), nil
}

func loadReadings(dir string) ([]Reading, int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, 0, fmt.Errorf("no CSV files found in %s", dir)
	}

	seen := map[time.Time]Reading{}
	skipped := 0
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return nil, skipped, err
		}
		readings, err := parseCSV(f)
		closeErr := f.Close()
		if err != nil {
			return nil, skipped, fmt.Errorf("%s: %w", file, err)
		}
		if closeErr != nil {
			return nil, skipped, closeErr
		}
		for _, reading := range readings {
			if existing, ok := seen[reading.Start]; ok {
				if math.Abs(existing.KWh-reading.KWh) > 0.000001 {
					return nil, skipped, fmt.Errorf("conflicting duplicate reading for %s: %.3f vs %.3f", reading.Start.Format(time.RFC3339), existing.KWh, reading.KWh)
				}
				skipped++
				continue
			}
			seen[reading.Start] = reading
		}
	}

	readings := make([]Reading, 0, len(seen))
	for _, reading := range seen {
		readings = append(readings, reading)
	}
	sort.Slice(readings, func(i, j int) bool {
		return readings[i].Start.Before(readings[j].Start)
	})
	return readings, skipped, nil
}

func parsePastedCSVs(input string) ([]Reading, int, error) {
	input = normalizePastedCSVText(input)
	blocks := splitCSVBlocks(input)
	if len(blocks) == 0 {
		return nil, 0, fmt.Errorf("no CSV header found; expected columns like Data; Strefa; Wartość kWh;Rodzaj")
	}
	var all []Reading
	for i, block := range blocks {
		readings, err := parseCSV(strings.NewReader(block))
		if err != nil {
			return nil, 0, fmt.Errorf("CSV block %d: %w", i+1, err)
		}
		all = append(all, readings...)
	}
	return dedupeReadings(all)
}

func normalizePastedCSVText(input string) string {
	input = strings.TrimSpace(input)
	if unquoted, err := strconv.Unquote(input); err == nil {
		input = unquoted
	}
	input = strings.ReplaceAll(input, "\\r\\n", "\n")
	input = strings.ReplaceAll(input, "\\n", "\n")
	input = strings.ReplaceAll(input, "\\t", "\t")
	input = strings.TrimPrefix(input, "\ufeff")
	return strings.TrimSpace(input)
}

func splitCSVBlocks(input string) []string {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var blocks []string
	var current strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(normalizeHeader(trimmed), "data;") && current.Len() > 0 {
			blocks = append(blocks, current.String())
			current.Reset()
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	if current.Len() > 0 {
		blocks = append(blocks, current.String())
	}
	return blocks
}

func dedupeReadings(readings []Reading) ([]Reading, int, error) {
	seen := map[time.Time]Reading{}
	skipped := 0
	for _, reading := range readings {
		if existing, ok := seen[reading.Start]; ok {
			if math.Abs(existing.KWh-reading.KWh) > 0.000001 {
				return nil, skipped, fmt.Errorf("conflicting duplicate reading for %s: %.3f vs %.3f", reading.Start.Format(time.RFC3339), existing.KWh, reading.KWh)
			}
			skipped++
			continue
		}
		seen[reading.Start] = reading
	}

	unique := make([]Reading, 0, len(seen))
	for _, reading := range seen {
		unique = append(unique, reading)
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].Start.Before(unique[j].Start)
	})
	return unique, skipped, nil
}

func parseCSV(r io.Reader) ([]Reading, error) {
	reader := csv.NewReader(r)
	reader.Comma = ';'
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, err
	}
	cols := columnIndexes(header)
	required := []string{"data", "wartosc", "rodzaj"}
	for _, name := range required {
		if _, ok := cols[name]; !ok {
			return nil, fmt.Errorf("missing required column %q", name)
		}
	}

	var readings []Reading
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(row) <= cols["data"] || strings.TrimSpace(row[cols["data"]]) == "" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(row[cols["rodzaj"]]))
		if kind != "pobór" && kind != "pobor" {
			continue
		}
		start, err := parsePolishMeterHour(row[cols["data"]])
		if err != nil {
			return nil, err
		}
		kwh, err := parseDecimal(row[cols["wartosc"]])
		if err != nil {
			return nil, err
		}
		readings = append(readings, Reading{Start: start, KWh: kwh})
	}
	return readings, nil
}

func columnIndexes(header []string) map[string]int {
	cols := map[string]int{}
	for i, col := range header {
		key := normalizeHeader(col)
		switch {
		case key == "data":
			cols["data"] = i
		case strings.Contains(key, "wartosc"):
			cols["wartosc"] = i
		case key == "rodzaj":
			cols["rodzaj"] = i
		}
	}
	return cols
}

func normalizeHeader(s string) string {
	replacer := strings.NewReplacer("ł", "l", "Ł", "l", "ś", "s", "Ś", "s", "ć", "c", "Ć", "c", "ź", "z", "Ź", "z", "ż", "z", "Ż", "z", "ó", "o", "Ó", "o", "ą", "a", "Ą", "a", "ę", "e", "Ę", "e", "ń", "n", "Ń", "n")
	return strings.ToLower(strings.TrimSpace(replacer.Replace(strings.TrimSuffix(s, ";"))))
}

func parsePolishMeterHour(s string) (time.Time, error) {
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
	}
	date, err := time.ParseInLocation("2006-01-02", parts[0], time.Local)
	if err != nil {
		return time.Time{}, err
	}
	hm := strings.Split(parts[1], ":")
	if len(hm) != 2 {
		return time.Time{}, fmt.Errorf("invalid hour %q", parts[1])
	}
	hour, err := strconv.Atoi(hm[0])
	if err != nil {
		return time.Time{}, err
	}
	if hour < 1 || hour > 24 {
		return time.Time{}, fmt.Errorf("hour must be 1..24 in %q", s)
	}
	return date.Add(time.Duration(hour-1) * time.Hour), nil
}

func parseDecimal(s string) (float64, error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	return strconv.ParseFloat(cleaned, 64)
}

func projectYear(readings []Reading, year int) []Reading {
	byWeekdayHour := map[string]profileBucket{}
	byHour := map[int]profileBucket{}
	all := profileBucket{}
	for _, reading := range readings {
		key := weekdayHourKey(reading.Start.Weekday(), reading.Start.Hour())
		byWeekdayHour[key] = addBucket(byWeekdayHour[key], reading.KWh)
		byHour[reading.Start.Hour()] = addBucket(byHour[reading.Start.Hour()], reading.KWh)
		all = addBucket(all, reading.KWh)
	}

	var projection []Reading
	for t := time.Date(year, 1, 1, 0, 0, 0, 0, time.Local); t.Year() == year; t = t.Add(time.Hour) {
		kwh := avgFor(t, byWeekdayHour, byHour, all)
		projection = append(projection, Reading{Start: t, KWh: kwh})
	}
	return projection
}

func addBucket(bucket profileBucket, kwh float64) profileBucket {
	bucket.sum += kwh
	bucket.count++
	return bucket
}

func avgFor(t time.Time, byWeekdayHour map[string]profileBucket, byHour map[int]profileBucket, all profileBucket) float64 {
	if bucket := byWeekdayHour[weekdayHourKey(t.Weekday(), t.Hour())]; bucket.count > 0 {
		return bucket.sum / float64(bucket.count)
	}
	if bucket := byHour[t.Hour()]; bucket.count > 0 {
		return bucket.sum / float64(bucket.count)
	}
	return all.sum / float64(all.count)
}

func weekdayHourKey(day time.Weekday, hour int) string {
	return fmt.Sprintf("%d-%02d", day, hour)
}

func buildTariffs(phase int, billingMonths int) []Tariff {
	fixed := fixedFees(phase, billingMonths)
	return []Tariff{
		{
			Name:  "G11",
			Fixed: fixed,
			Zones: []Zone{{Name: "all-day", EnergyGross: 0.7712, DistributionNet: 0.2464, Match: always}},
		},
		{
			Name:  "G12",
			Fixed: fixed,
			Zones: []Zone{
				{Name: "day", EnergyGross: 0.7712, DistributionNet: 0.2841, Match: g12Day},
				{Name: "night", EnergyGross: 0.5141, DistributionNet: 0.0558, Match: always},
			},
		},
		{
			Name:  "G12w",
			Fixed: fixed,
			Zones: []Zone{
				{Name: "peak", EnergyGross: 0.7712, DistributionNet: 0.3298, Match: g12wPeak},
				{Name: "off-peak", EnergyGross: 0.5141, DistributionNet: 0.0512, Match: always},
			},
		},
		{
			Name:  "G13",
			Fixed: fixed,
			Zones: []Zone{
				{Name: "morning-peak", EnergyGross: 0.5803, DistributionNet: 0.2203, Match: g13MorningPeak},
				{Name: "afternoon-peak", EnergyGross: 0.9631, DistributionNet: 0.3898, Match: g13AfternoonPeak},
				{Name: "other", EnergyGross: 0.5240, DistributionNet: 0.0392, Match: always},
			},
		},
	}
}

func fixedFees(phase int, billingMonths int) FixedFees {
	distribution := 10.86
	if phase == 1 {
		distribution = 7.38
	}
	sellerGross, _ := sellerSubscriptionGrossByBillingCycle(billingMonths)
	return FixedFees{
		DistributionFixedNetPerKWMonth:   distribution,
		DistributionSubscriptionNetMonth: distribution,
		SellerSubscriptionGrossMonth:     sellerGross,
	}
}

func sellerSubscriptionGrossByBillingCycle(months int) (float64, bool) {
	sellerGross := map[int]float64{12: 0.47, 6: 0.93, 2: 2.80, 1: 5.61}
	value, ok := sellerGross[months]
	return value, ok
}

func calculateTariff(tariff Tariff, readings []Reading, powerKW float64) TariffResult {
	result := TariffResult{
		Name:  tariff.Name,
		Zones: map[string]ZoneUsage{},
	}
	for _, reading := range readings {
		zone := findZone(tariff, reading.Start)
		variableNet := reading.KWh * (zone.EnergyGross/(1+vatRate) + zone.DistributionNet + qualityFeeNet + ozeFeeNet + cogenFeeNet + capacityNet)
		usage := result.Zones[zone.Name]
		usage.KWh += reading.KWh
		usage.NetCost += variableNet
		usage.GrossCost += variableNet * (1 + vatRate)
		result.Zones[zone.Name] = usage
		result.TotalKWh += reading.KWh
		result.VariableNet += variableNet
	}
	result.DistributionFixedNet = 12 * (powerKW*tariff.Fixed.DistributionFixedNetPerKWMonth + tariff.Fixed.DistributionSubscriptionNetMonth)
	result.SellerSubscriptionGross = 12 * tariff.Fixed.SellerSubscriptionGrossMonth
	result.NetTotal = result.VariableNet + result.DistributionFixedNet
	result.GrossTotal = result.NetTotal*(1+vatRate) + result.SellerSubscriptionGross
	return result
}

func findZone(tariff Tariff, t time.Time) Zone {
	for _, zone := range tariff.Zones {
		if zone.Match(t) {
			return zone
		}
	}
	panic("tariff has no matching zone")
}

func always(time.Time) bool {
	return true
}

func g12Day(t time.Time) bool {
	h := t.Hour()
	return (h >= 6 && h < 13) || (h >= 15 && h < 22)
}

func g12wPeak(t time.Time) bool {
	if isWeekend(t) {
		return false
	}
	return g12Day(t)
}

func g13MorningPeak(t time.Time) bool {
	if isWeekend(t) {
		return false
	}
	h := t.Hour()
	return h >= 7 && h < 13
}

func g13AfternoonPeak(t time.Time) bool {
	if isWeekend(t) {
		return false
	}
	h := t.Hour()
	if t.Month() >= time.April && t.Month() <= time.September {
		return h >= 19 && h < 22
	}
	return h >= 16 && h < 21
}

func isWeekend(t time.Time) bool {
	return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
}

func buildInputSummary(readings []Reading, projection []Reading, year int, skipped int) inputSummary {
	var observed float64
	for _, reading := range readings {
		observed += reading.KWh
	}
	var projected float64
	for _, reading := range projection {
		projected += reading.KWh
	}
	return inputSummary{
		Readings:       len(readings),
		Duplicates:     skipped,
		ObservedKWh:    observed,
		ProjectedKWh:   projected,
		ProjectedYear:  year,
		ProjectedHours: len(projection),
		RangeStart:     readings[0].Start.Format("2006-01-02 15:04"),
		RangeEnd:       readings[len(readings)-1].Start.Format("2006-01-02 15:04"),
	}
}

func weekdayProfiles(readings []Reading) []weekdayProfile {
	sums := map[time.Weekday]profileBucket{}
	for _, reading := range readings {
		sums[reading.Start.Weekday()] = addBucket(sums[reading.Start.Weekday()], reading.KWh)
	}
	order := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	out := make([]weekdayProfile, 0, len(order))
	for _, day := range order {
		bucket := sums[day]
		if bucket.count == 0 {
			out = append(out, weekdayProfile{Name: weekdayShortName(day), KWh: 0})
			continue
		}
		out = append(out, weekdayProfile{Name: weekdayShortName(day), KWh: bucket.sum / float64(bucket.count)})
	}
	return out
}

func hourProfiles(readings []Reading) []hourProfile {
	sums := map[int]profileBucket{}
	for _, reading := range readings {
		sums[reading.Start.Hour()] = addBucket(sums[reading.Start.Hour()], reading.KWh)
	}
	out := make([]hourProfile, 0, 24)
	for hour := 0; hour < 24; hour++ {
		bucket := sums[hour]
		kwh := 0.0
		if bucket.count > 0 {
			kwh = bucket.sum / float64(bucket.count)
		}
		out = append(out, hourProfile{Hour: fmt.Sprintf("%02d", hour), KWh: kwh})
	}
	return out
}

func weekdayShortName(day time.Weekday) string {
	switch day {
	case time.Monday:
		return "Mon"
	case time.Tuesday:
		return "Tue"
	case time.Wednesday:
		return "Wed"
	case time.Thursday:
		return "Thu"
	case time.Friday:
		return "Fri"
	case time.Saturday:
		return "Sat"
	default:
		return "Sun"
	}
}

func sortedZoneNames(zones map[string]ZoneUsage) []string {
	names := make([]string, 0, len(zones))
	for name := range zones {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func zoneShare(kwh float64, total float64) string {
	if total == 0 {
		return "0%"
	}
	return formatPercent(kwh / total)
}

func formatMoney(v float64) string {
	return fmt.Sprintf("%.2f PLN", v)
}

func formatKWh(v float64) string {
	return fmt.Sprintf("%.2f kWh", v)
}

func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

func barWidth(v float64) string {
	width := v * 55
	if width < 3 && v > 0 {
		width = 3
	}
	if width > 100 {
		width = 100
	}
	return fmt.Sprintf("%.1f%%", width)
}

func htmlSafeID(s string) string {
	s = strings.ToLower(s)
	replacer := strings.NewReplacer(" ", "-", "_", "-", "/", "-", ".", "-", ":", "-")
	return replacer.Replace(s)
}

func jsString(s string) template.JS {
	b, err := json.Marshal(s)
	if err != nil {
		return template.JS(`""`)
	}
	return template.JS(b)
}

func printInputSummary(readings []Reading, projection []Reading, year int, skipped int) {
	summary := buildInputSummary(readings, projection, year, skipped)
	fmt.Printf("Input\n")
	fmt.Printf("  readings: %d hourly records (%d duplicate records skipped)\n", summary.Readings, summary.Duplicates)
	fmt.Printf("  observed range: %s to %s\n", summary.RangeStart, summary.RangeEnd)
	fmt.Printf("  observed usage: %.2f kWh\n", summary.ObservedKWh)
	fmt.Printf("  projected year: %d (%d hours), %.2f kWh\n\n", summary.ProjectedYear, summary.ProjectedHours, summary.ProjectedKWh)
}

func printResults(results []TariffResult) {
	best := results[0].GrossTotal
	fmt.Printf("Tariff comparison, projected annual cost\n")
	fmt.Printf("%-6s %12s %12s %12s %12s\n", "Tariff", "kWh", "net PLN", "gross PLN", "vs best")
	for _, result := range results {
		fmt.Printf("%-6s %12.2f %12.2f %12.2f %11.2f\n", result.Name, result.TotalKWh, result.NetTotal, result.GrossTotal, result.GrossTotal-best)
	}
	fmt.Println()
	for _, result := range results {
		fmt.Printf("%s zones\n", result.Name)
		names := make([]string, 0, len(result.Zones))
		for name := range result.Zones {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			usage := result.Zones[name]
			fmt.Printf("  %-16s %10.2f kWh %10.2f PLN gross\n", name, usage.KWh, usage.GrossCost)
		}
		fmt.Printf("  %-16s %10s     %10.2f PLN gross\n", "fixed distribution", "", result.DistributionFixedNet*(1+vatRate))
		fmt.Printf("  %-16s %10s     %10.2f PLN gross\n", "seller subscription", "", result.SellerSubscriptionGross)
		fmt.Println()
	}
}

func printHourlyProfile(readings []Reading) {
	byWeekdayHour := map[string]profileBucket{}
	for _, reading := range readings {
		key := weekdayHourKey(reading.Start.Weekday(), reading.Start.Hour())
		byWeekdayHour[key] = addBucket(byWeekdayHour[key], reading.KWh)
	}
	fmt.Println("Observed average profile by weekday/hour")
	for day := time.Sunday; day <= time.Saturday; day++ {
		for hour := 0; hour < 24; hour++ {
			bucket := byWeekdayHour[weekdayHourKey(day, hour)]
			if bucket.count == 0 {
				continue
			}
			fmt.Printf("  %-9s %02d:00 %.3f kWh (%d samples)\n", day, hour, bucket.sum/float64(bucket.count), bucket.count)
		}
	}
}

func exitErr(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

const appTemplate = `{{define "page"}}
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Tariff Comparator</title>
  <script src="https://unpkg.com/htmx.org@1.9.12"></script>
  <style>
    :root {
      --paper: #f7f3ea;
      --ink: #171916;
      --muted: #64685f;
      --line: #d9d0c0;
      --panel: #fffaf1;
      --coal: #20231e;
      --green: #2f6f4e;
      --blue: #315f7d;
      --amber: #c0832e;
      --red: #a94a3a;
      --shadow: 0 22px 60px rgba(32, 35, 30, 0.12);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      color: var(--ink);
      background:
        linear-gradient(90deg, rgba(23,25,22,.045) 1px, transparent 1px) 0 0 / 42px 42px,
        linear-gradient(0deg, rgba(23,25,22,.035) 1px, transparent 1px) 0 0 / 42px 42px,
        var(--paper);
      font: 15px/1.45 Charter, "Iowan Old Style", Georgia, serif;
    }
    button, input, select, textarea { font: inherit; }
    .shell { width: min(1440px, calc(100% - 32px)); margin: 0 auto; }
    .mast {
      min-height: 210px;
      display: grid;
      grid-template-columns: minmax(0, 1.1fr) minmax(320px, .9fr);
      gap: 32px;
      align-items: end;
      padding: 34px 0 22px;
      border-bottom: 2px solid var(--coal);
    }
    h1 {
      font-size: clamp(44px, 7vw, 104px);
      line-height: .86;
      margin: 0;
      max-width: 920px;
      letter-spacing: 0;
      font-weight: 800;
    }
    .lead {
      margin: 0 0 6px;
      color: var(--coal);
      font-size: 18px;
      max-width: 640px;
    }
    .layout {
      display: grid;
      grid-template-columns: 420px minmax(0, 1fr);
      gap: 24px;
      align-items: start;
      padding: 24px 0 44px;
    }
    .control-panel {
      position: sticky;
      top: 18px;
      background: var(--coal);
      color: #fff8eb;
      border: 2px solid var(--coal);
      box-shadow: var(--shadow);
      padding: 18px;
    }
    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 12px;
      margin-bottom: 14px;
    }
    label {
      display: grid;
      gap: 6px;
      color: #e7dfd0;
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: .08em;
    }
    input, select, textarea {
      width: 100%;
      border: 1px solid rgba(255,255,255,.28);
      background: #fffaf1;
      color: var(--ink);
      border-radius: 4px;
      padding: 10px 11px;
      outline: none;
    }
    input:focus, select:focus, textarea:focus { box-shadow: 0 0 0 3px rgba(192,131,46,.35); }
    textarea {
      min-height: 380px;
      resize: vertical;
      font: 12px/1.35 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      white-space: pre;
    }
    .actions { display: flex; gap: 10px; align-items: center; margin-top: 12px; flex-wrap: wrap; }
    .primary, .secondary {
      border: 1px solid rgba(255,255,255,.28);
      border-radius: 4px;
      cursor: pointer;
      min-height: 42px;
      padding: 0 14px;
      font-weight: 700;
    }
    .primary { background: var(--amber); color: #18140c; }
    .secondary { background: transparent; color: #fff8eb; }
    .hint { color: #bdb5a5; font-size: 13px; margin: 12px 0 0; }
    .htmx-indicator { opacity: 0; transition: opacity .2s ease; color: #f2d69e; }
    .htmx-request .htmx-indicator { opacity: 1; }
    .results { display: grid; gap: 18px; }
    .empty {
      min-height: 560px;
      border: 2px dashed var(--line);
      display: grid;
      place-items: center;
      padding: 36px;
      background: rgba(255,250,241,.68);
    }
    .empty p { max-width: 520px; margin: 0; color: var(--muted); font-size: 18px; }
    .error {
      border-left: 8px solid var(--red);
      background: #fff7f3;
      padding: 16px 18px;
      font-weight: 700;
    }
    .kpi-grid {
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 12px;
    }
    .kpi {
      background: var(--panel);
      border: 1px solid var(--line);
      padding: 15px;
      min-height: 104px;
    }
    .kpi b { display: block; font-size: 23px; line-height: 1.05; margin-top: 6px; overflow-wrap: anywhere; }
    .kpi span { color: var(--muted); text-transform: uppercase; letter-spacing: .08em; font-size: 11px; }
    .winner {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 18px;
      align-items: end;
      color: #f8f0df;
      background: var(--green);
      padding: 20px;
      border: 2px solid #1d4932;
    }
    .winner h2 { margin: 0; font-size: clamp(26px, 4vw, 54px); line-height: .95; }
    .winner .price { font-size: clamp(30px, 5vw, 62px); font-weight: 800; text-align: right; line-height: .9; }
    .winner small { display: block; color: #dce9dc; margin-top: 8px; }
    .tariff-list { display: grid; gap: 10px; }
    .tariff-row {
      display: grid;
      grid-template-columns: 82px minmax(180px, 1fr) 160px 135px;
      gap: 14px;
      align-items: center;
      background: var(--panel);
      border: 1px solid var(--line);
      padding: 14px;
    }
    .rank {
      width: 54px;
      height: 54px;
      display: grid;
      place-items: center;
      border-radius: 50%;
      background: var(--coal);
      color: #fff8eb;
      font-weight: 800;
      font-size: 20px;
    }
    .tariff-row h3 { margin: 0 0 5px; font-size: 24px; }
    .tariff-row p { margin: 0; color: var(--muted); }
    .money { font-size: 22px; font-weight: 800; text-align: right; }
    .delta { color: var(--red); text-align: right; font-weight: 700; }
    .zones {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 14px;
    }
    .zone-block {
      background: var(--panel);
      border: 1px solid var(--line);
      padding: 14px;
    }
    .zone-block h3 { margin: 0 0 12px; font-size: 22px; }
    .zone-line {
      display: grid;
      grid-template-columns: minmax(110px, 1fr) 1.1fr 90px;
      gap: 10px;
      align-items: center;
      padding: 7px 0;
      border-top: 1px solid var(--line);
    }
    .bar {
      height: 12px;
      background: #e5ddcf;
      border: 1px solid #cfc4b2;
      overflow: hidden;
    }
    .bar i { display: block; height: 100%; background: var(--blue); }
    .profile {
      display: grid;
      grid-template-columns: .8fr 1.2fr;
      gap: 14px;
    }
    .profile-block {
      background: var(--panel);
      border: 1px solid var(--line);
      padding: 14px;
    }
    .profile-block h3 { margin: 0 0 12px; font-size: 22px; }
    .weekday-bars { display: grid; gap: 8px; }
    .weekday-bar {
      display: grid;
      grid-template-columns: 42px minmax(0, 1fr) 82px;
      gap: 10px;
      align-items: center;
    }
    .hour-grid {
      display: grid;
      grid-template-columns: repeat(12, minmax(0, 1fr));
      gap: 6px;
    }
    .hour-cell {
      border: 1px solid var(--line);
      min-height: 48px;
      padding: 6px 4px;
      text-align: center;
      background: #f3eadb;
    }
    .hour-cell b { display: block; font-size: 11px; color: var(--muted); }
    .hour-cell span { font-size: 12px; font-weight: 800; }
    .assumptions {
      background: #eef4ef;
      border: 1px solid #bdd0c0;
      padding: 14px 18px;
    }
    .assumptions h3 { margin: 0 0 8px; }
    .assumptions ul { margin: 0; padding-left: 18px; }
    .assumptions li { margin: 5px 0; }
    @media (max-width: 1050px) {
      .mast, .layout, .profile { grid-template-columns: 1fr; }
      .control-panel { position: static; }
      .kpi-grid, .zones { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    }
    @media (max-width: 720px) {
      .shell { width: min(100% - 20px, 1440px); }
      .mast { min-height: 0; padding-top: 22px; }
      .form-grid, .kpi-grid, .zones, .winner { grid-template-columns: 1fr; }
      .winner .price { text-align: left; }
      .tariff-row { grid-template-columns: 52px minmax(0, 1fr); }
      .money, .delta { text-align: left; }
      .hour-grid { grid-template-columns: repeat(6, minmax(0, 1fr)); }
      textarea { min-height: 300px; }
    }
  </style>
</head>
<body>
  <header class="shell mast">
    <h1>Tariff Comparator</h1>
    <p class="lead">Paste TAURON hourly CSV exports, project the full year from weekday and hour patterns, then compare G11, G12, G12w, and G13 with fixed fees included.</p>
  </header>

  <main class="shell layout">
    <form class="control-panel" method="post" action="/compare" hx-post="/compare" hx-target="#results" hx-swap="innerHTML">
      <div class="form-grid">
        <label>Year
          <input name="year" type="number" min="2020" max="2040" value="{{.Form.Year}}">
        </label>
        <label>Phase
          <select name="phase">
            <option value="3" {{if eq .Form.Phase 3}}selected{{end}}>3-phase</option>
            <option value="1" {{if eq .Form.Phase 1}}selected{{end}}>1-phase</option>
          </select>
        </label>
        <label>Power kW
          <input name="powerKW" inputmode="decimal" value="{{printf "%.2f" .Form.PowerKW}}">
        </label>
        <label>Billing
          <select name="billingMonths">
            <option value="12" {{if eq .Form.BillingMonths 12}}selected{{end}}>12 months</option>
            <option value="6" {{if eq .Form.BillingMonths 6}}selected{{end}}>6 months</option>
            <option value="2" {{if eq .Form.BillingMonths 2}}selected{{end}}>2 months</option>
            <option value="1" {{if eq .Form.BillingMonths 1}}selected{{end}}>1 month</option>
          </select>
        </label>
      </div>
      <label>CSV exports
        <textarea id="csv-input" name="csv" spellcheck="false" autocomplete="off">{{.Form.CSV}}</textarea>
      </label>
      <div class="actions">
        <button class="primary" type="submit">Compare tariffs</button>
        <button class="secondary" type="button" onclick="document.getElementById('csv-input').value = sampleCSV">Load sample</button>
        <span class="htmx-indicator">Calculating...</span>
      </div>
      <p class="hint">Multiple copied CSV files can be pasted one after another. Matching duplicate hours are skipped.</p>
    </form>

    <section id="results" class="results">
      {{template "results" .}}
    </section>
  </main>
  <script>
    const sampleCSV = {{jsString .SampleCSV}};
  </script>
</body>
</html>
{{end}}

{{define "results"}}
  {{if .Error}}
    <div class="error">{{.Error}}</div>
  {{else if .Report}}
    <section class="winner">
      <div>
        <h2>{{.Report.Best.Name}} is cheapest for this profile</h2>
        <small>{{money .Report.SavingsVsWorst}} less than the most expensive tariff in the projection</small>
      </div>
      <div class="price">{{money .Report.Best.GrossTotal}}</div>
    </section>

    <section class="kpi-grid" aria-label="Input summary">
      <div class="kpi"><span>Projected usage</span><b>{{kwh .Report.Input.ProjectedKWh}}</b></div>
      <div class="kpi"><span>Observed usage</span><b>{{kwh .Report.Input.ObservedKWh}}</b></div>
      <div class="kpi"><span>Hourly records</span><b>{{.Report.Input.Readings}}</b></div>
      <div class="kpi"><span>Observed range</span><b>{{.Report.Input.RangeStart}}<br>{{.Report.Input.RangeEnd}}</b></div>
    </section>

    <section class="tariff-list" aria-label="Tariff ranking">
      {{range $idx, $result := .Report.Results}}
        <article class="tariff-row">
          <div class="rank">{{rank $idx}}</div>
          <div>
            <h3>{{$result.Name}}</h3>
            <p>{{kwh $result.TotalKWh}} projected, {{money $result.NetTotal}} net before seller subscription gross add-on</p>
          </div>
          <div class="money">{{money $result.GrossTotal}}</div>
          <div class="delta">{{if eq $idx 0}}best{{else}}+{{money (minus $result.GrossTotal $.Report.Best.GrossTotal)}}{{end}}</div>
        </article>
      {{end}}
    </section>

    <section class="zones">
      {{range $result := .Report.Results}}
        <article class="zone-block">
          <h3>{{$result.Name}} zones</h3>
          {{range $zoneName := zoneNames $result.Zones}}
            {{$usage := index $result.Zones $zoneName}}
            <div class="zone-line">
              <strong>{{$zoneName}}</strong>
              <div class="bar" title="{{zoneShare $usage.KWh $result.TotalKWh}}"><i style="width: {{zoneShare $usage.KWh $result.TotalKWh}}"></i></div>
              <span>{{kwh $usage.KWh}}</span>
            </div>
          {{end}}
          <div class="zone-line">
            <strong>fixed distribution</strong>
            <div class="bar"><i style="width: 100%"></i></div>
            <span>{{money (mul $result.DistributionFixedNet 1.23)}}</span>
          </div>
          <div class="zone-line">
            <strong>seller subscription</strong>
            <div class="bar"><i style="width: 100%"></i></div>
            <span>{{money $result.SellerSubscriptionGross}}</span>
          </div>
        </article>
      {{end}}
    </section>

    <section class="profile">
      <article class="profile-block">
        <h3>Average kWh by weekday</h3>
        <div class="weekday-bars">
          {{range .Report.WeekdayProfiles}}
            <div class="weekday-bar">
              <strong>{{.Name}}</strong>
              <div class="bar"><i style="width: {{barWidth .KWh}}"></i></div>
              <span>{{printf "%.3f" .KWh}}</span>
            </div>
          {{end}}
        </div>
      </article>
      <article class="profile-block">
        <h3>Average kWh by hour</h3>
        <div class="hour-grid">
          {{range .Report.HourProfiles}}
            <div class="hour-cell">
              <b>{{.Hour}}:00</b>
              <span>{{printf "%.2f" .KWh}}</span>
            </div>
          {{end}}
        </div>
      </article>
    </section>

    <section class="assumptions">
      <h3>Calculation assumptions</h3>
      <ul>
        {{range .Report.Assumptions}}<li>{{.}}</li>{{end}}
      </ul>
    </section>
  {{else}}
    <div class="empty">
      <p>Paste CSV data or use the bundled sample, then run the comparison. Results will appear here without reloading the whole page when HTMX is available.</p>
    </div>
  {{end}}
{{end}}`
