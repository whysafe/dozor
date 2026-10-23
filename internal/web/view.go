package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/probe"
	"github.com/whysafe/dozor/internal/store"
	"github.com/whysafe/dozor/internal/textru"
)

const (
	historyDays  = 90
	sparkBuckets = 48
	sparkWidth   = 240
	sparkHeight  = 40
)

// Monitor states shared by the page, the CSS classes and the JSON API.
const (
	stateUp       = "up"
	stateDegraded = "degraded"
	stateDown     = "down"
	stateUnknown  = "unknown"
)

type pageData struct {
	Title     string
	CSS       template.CSS
	Refresh   int
	Generated string
	Overall   overallView
	Groups    []groupView
	Incidents []incidentView
	FirstDay  string
}

type overallView struct {
	State string
	Text  string
	Note  string
}

type groupView struct {
	Name     string
	Monitors []monitorView
}

type monitorView struct {
	ID         string
	Name       string
	Target     string
	State      string
	StateText  string
	Message    string
	Latency    string
	Uptime24   string
	Uptime30   string
	Uptime90   string
	Days       []dayView
	Spark      string
	SparkArea  string
	SparkRange string
	api        apiMonitor
}

type dayView struct {
	Class string
	Title string
}

type incidentView struct {
	Monitor  string
	Cause    string
	Started  string
	Duration string
	Open     bool
}

func (s *Server) buildPage(ctx context.Context) (pageData, error) {
	now := s.now()
	loc := s.store.Location()
	data := pageData{
		Title:     s.cfg.Title,
		CSS:       s.css,
		Refresh:   s.Refresh,
		Generated: textru.DateTime(now.In(loc)),
		FirstDay:  textru.Date(now.In(loc).AddDate(0, 0, -(historyDays - 1))),
	}

	groupIndex := map[string]int{}
	counts := map[string]int{}
	for _, m := range s.cfg.Monitors {
		view, err := s.buildMonitor(ctx, m, now)
		if err != nil {
			return pageData{}, err
		}
		counts[view.State]++
		name := m.Group
		if name == "" {
			name = "Сервисы"
		}
		i, ok := groupIndex[name]
		if !ok {
			i = len(data.Groups)
			groupIndex[name] = i
			data.Groups = append(data.Groups, groupView{Name: name})
		}
		data.Groups[i].Monitors = append(data.Groups[i].Monitors, view)
	}
	data.Overall = overall(counts, len(s.cfg.Monitors))

	incidents, err := s.store.Incidents(ctx, now.AddDate(0, 0, -30), 20)
	if err != nil {
		return pageData{}, err
	}
	for _, inc := range incidents {
		name := inc.MonitorID
		if m, ok := s.cfg.MonitorByID(inc.MonitorID); ok {
			name = m.Name
		}
		data.Incidents = append(data.Incidents, incidentView{
			Monitor:  name,
			Cause:    inc.Cause,
			Started:  textru.DateTime(inc.StartedAt.In(loc)),
			Duration: textru.Duration(inc.Duration(now)),
			Open:     inc.Open(),
		})
	}
	return data, nil
}

func overall(counts map[string]int, total int) overallView {
	down := counts[stateDown]
	switch {
	case down > 0 && down == total:
		return overallView{stateDown, "Все сервисы недоступны", "Мы уже разбираемся."}
	case down > 0:
		return overallView{stateDown, "Частичный сбой", fmt.Sprintf("Недоступно: %d из %d.", down, total)}
	case counts[stateDegraded] > 0:
		return overallView{stateDegraded, "Есть замедления", "Сервисы отвечают, но не все в норме."}
	case counts[stateUnknown] == total:
		return overallView{stateUnknown, "Ждём первых проверок", "Данные появятся через минуту."}
	default:
		return overallView{stateUp, "Все системы работают", "Проверки проходят штатно."}
	}
}

func (s *Server) buildMonitor(ctx context.Context, m config.Monitor, now time.Time) (monitorView, error) {
	v := monitorView{ID: m.ID, Name: m.Name, Target: m.Target()}
	v.api = apiMonitor{ID: m.ID, Name: m.Name, Group: m.Group, Target: m.Target()}

	latest, has, err := s.store.Latest(ctx, m.ID)
	if err != nil {
		return v, err
	}
	_, incErr := s.store.OpenIncidentFor(ctx, m.ID)
	if incErr != nil && !errors.Is(incErr, store.ErrNotFound) {
		return v, incErr
	}
	v.State, v.StateText = currentState(latest, has, incErr == nil)
	if has {
		v.Message = latest.Message
		if latest.Status.Available() {
			v.Latency = fmt.Sprintf("%d мс", latest.Latency.Milliseconds())
		}
		ms := latest.Latency.Milliseconds()
		at := latest.At.UTC().Format(time.RFC3339)
		v.api.LatencyMS, v.api.CheckedAt, v.api.Message = &ms, &at, latest.Message
	}
	v.api.State = v.State

	days, err := s.store.Days(ctx, m.ID, now, historyDays)
	if err != nil {
		return v, err
	}
	v.Days = make([]dayView, len(days))
	for i, d := range days {
		v.Days[i] = dayBar(d, s.store.Location())
	}
	v.Uptime30, v.api.Uptime30d = uptimeText(days[historyDays-30:])
	v.Uptime90, v.api.Uptime90d = uptimeText(days)

	recent, err := s.store.Since(ctx, m.ID, now.Add(-24*time.Hour))
	if err != nil {
		return v, err
	}
	v.Uptime24, v.api.Uptime24h = uptimeOf(recent)
	v.Spark, v.SparkArea, v.SparkRange = sparkline(recent, now)
	return v, nil
}

func currentState(latest probe.Result, has, incidentOpen bool) (state, text string) {
	switch {
	case incidentOpen:
		return stateDown, "Недоступен"
	case !has:
		return stateUnknown, "Нет данных"
	case latest.Status == probe.Up:
		return stateUp, "Работает"
	case latest.Status == probe.Degraded:
		return stateDegraded, "Есть проблемы"
	default:
		// A failure that has not reached the threshold yet.
		return stateDegraded, "Сбои при проверке"
	}
}

func uptimeText(days []store.DayStat) (string, *float64) {
	ratio, ok := store.Uptime(days)
	if !ok {
		return "—", nil
	}
	return textru.Percent(ratio), &ratio
}

func uptimeOf(results []probe.Result) (string, *float64) {
	if len(results) == 0 {
		return "—", nil
	}
	available := 0
	for _, r := range results {
		if r.Status.Available() {
			available++
		}
	}
	ratio := float64(available) / float64(len(results))
	return textru.Percent(ratio), &ratio
}

func dayBar(d store.DayStat, loc *time.Location) dayView {
	day, _ := time.ParseInLocation(time.DateOnly, d.Day, loc)
	label := textru.Date(day)
	if d.Total == 0 {
		return dayView{Class: "none", Title: label + " — нет данных"}
	}
	up := d.Uptime()
	class := "ok"
	switch {
	case up < 0.95:
		class = "bad"
	case up < 0.98:
		class = "warn"
	case up < 1 || d.Degraded*10 > d.Total:
		class = "minor"
	}
	failed := int64(d.Total - d.Available)
	title := fmt.Sprintf("%s — %s, %d %s из %d", label, textru.Percent(up),
		failed, textru.Plural(failed, "сбой", "сбоя", "сбоев"), d.Total)
	if d.AvgLatency > 0 {
		title += fmt.Sprintf(", в среднем %d мс", d.AvgLatency.Milliseconds())
	}
	return dayView{Class: class, Title: title}
}

// sparkline averages the last 24 hours of latency into fixed buckets and
// returns the polyline points, a closed area path and the min–max caption.
func sparkline(results []probe.Result, now time.Time) (line, area, caption string) {
	var sums [sparkBuckets]float64
	var counts [sparkBuckets]int
	start := now.Add(-24 * time.Hour)
	bucket := 24 * time.Hour / sparkBuckets
	for _, r := range results {
		if !r.Status.Available() {
			continue
		}
		i := int(r.At.Sub(start) / bucket)
		if i < 0 || i >= sparkBuckets {
			continue
		}
		sums[i] += float64(r.Latency.Milliseconds())
		counts[i]++
	}

	lo, hi := math.Inf(1), math.Inf(-1)
	var avg [sparkBuckets]float64
	for i := range avg {
		if counts[i] == 0 {
			avg[i] = math.NaN()
			continue
		}
		avg[i] = sums[i] / float64(counts[i])
		lo, hi = min(lo, avg[i]), max(hi, avg[i])
	}
	if math.IsInf(lo, 1) {
		return "", "", ""
	}
	span := max(hi-lo, 1)

	var pts []string
	firstX, lastX := -1.0, 0.0
	for i, a := range avg {
		if math.IsNaN(a) {
			continue
		}
		x := float64(i) * sparkWidth / (sparkBuckets - 1)
		y := 4 + (1-(a-lo)/span)*(sparkHeight-8)
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
		if firstX < 0 {
			firstX = x
		}
		lastX = x
	}
	line = strings.Join(pts, " ")
	area = fmt.Sprintf("M%.1f,%d L%s L%.1f,%d Z", firstX, sparkHeight, strings.Join(pts, " L"), lastX, sparkHeight)
	caption = fmt.Sprintf("%.0f–%.0f мс", lo, hi)
	return line, area, caption
}
