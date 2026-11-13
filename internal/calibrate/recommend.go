package calibrate

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Malko-opensource/costsplit/internal/bench"
)

// ArmStat is one setting measured on the tasks every setting finished.
type ArmStat struct {
	Arm        string
	N, Pass    int
	Mean       float64 // cost per task, USD
	Median     float64
	Turns      float64
	Lost       int     // tasks the baseline did and this setting did not
	Gained     int     // the reverse
	P          float64 // exact McNemar test against the baseline
	Acceptable bool
}

// Report is the outcome of a calibration.
type Report struct {
	Baseline  string
	Tasks     int // tasks every arm finished
	Tolerance int // net tasks a setting may lose and still count as "as good"
	Arms      []ArmStat
	Pick      string
	Errors    int
	Spent     float64
	Locate    bool // scored by edit location, not by tests
}

func mcnemar(b, c int) float64 {
	m := b + c
	if m == 0 {
		return 1
	}
	lo := b
	if c < b {
		lo = c
	}
	s := 0.0
	for i := 0; i <= lo; i++ {
		s += binom(m, i)
	}
	return math.Min(1, 2*s/math.Pow(2, float64(m)))
}

func binom(n, k int) float64 {
	r := 1.0
	for i := 1; i <= k; i++ {
		r = r * float64(n-k+i) / float64(i)
	}
	return r
}

// MinTasks is the fewest tasks a recommendation is stated on without a warning.
const MinTasks = 20

// Tolerance is how many tasks, net, a cheaper setting may lose before it
// stops counting as equal to the baseline: 5% of the tasks, at least one.
// With 45 tasks that is 2, inside the run-to-run noise we measured (about 3).
func Tolerance(n int) int {
	t := int(math.Round(0.05 * float64(n)))
	if t < 1 {
		t = 1
	}
	return t
}

// Analyze compares every arm with the baseline on the tasks all arms
// finished without error, and picks the cheapest arm that loses no more
// than the tolerance. Later results for the same task and arm win.
func Analyze(results []bench.Result, baseline string, tolerance int) Report {
	by := map[string]map[string]bench.Result{}
	var order []string
	rep := Report{Baseline: baseline}
	for _, r := range results {
		rep.Spent += r.Total.CostUSD
		if by[r.Arm] == nil {
			by[r.Arm] = map[string]bench.Result{}
			order = append(order, r.Arm)
		}
		by[r.Arm][r.Exercise] = r
	}
	base, ok := by[baseline]
	if !ok {
		return rep
	}
	var ids []string
	for id := range base {
		all := true
		for _, a := range order {
			if r, ok := by[a][id]; !ok || r.Outcome == "error" {
				all = false
			}
		}
		if all {
			ids = append(ids, id)
		}
	}
	for _, a := range order {
		for _, r := range by[a] {
			if r.Outcome == "error" {
				rep.Errors++
			}
		}
	}
	rep.Tasks = len(ids)
	if tolerance < 0 {
		tolerance = Tolerance(len(ids))
	}
	rep.Tolerance = tolerance
	if len(ids) == 0 {
		return rep
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i] == baseline && order[j] != baseline })
	best := -1.0
	for _, a := range order {
		st := ArmStat{Arm: a, N: len(ids)}
		costs := make([]float64, 0, len(ids))
		for _, id := range ids {
			r, b := by[a][id], base[id]
			if r.Pass {
				st.Pass++
			}
			if b.Pass && !r.Pass {
				st.Lost++
			}
			if r.Pass && !b.Pass {
				st.Gained++
			}
			costs = append(costs, r.Total.CostUSD)
			st.Mean += r.Total.CostUSD / float64(len(ids))
			if t, ok := r.Info["turns"].(float64); ok {
				st.Turns += t / float64(len(ids))
			} else if t, ok := r.Info["turns"].(int); ok {
				st.Turns += float64(t) / float64(len(ids))
			}
			if _, ok := r.Info["locate"]; ok {
				rep.Locate = true
			}
		}
		sort.Float64s(costs)
		st.Median = costs[len(costs)/2]
		if len(costs)%2 == 0 {
			st.Median = (costs[len(costs)/2-1] + costs[len(costs)/2]) / 2
		}
		st.P = mcnemar(st.Gained, st.Lost)
		st.Acceptable = st.Lost-st.Gained <= tolerance
		if st.Acceptable && (best < 0 || st.Mean < best) {
			best, rep.Pick = st.Mean, a
		}
		rep.Arms = append(rep.Arms, st)
	}
	return rep
}

var armNames = map[string]string{
	"agent-full": "큰 모델, 기본 추론량", "agent-low": "큰 모델, 추론량 low", "agent-medium": "큰 모델, 추론량 medium",
	"agent-haiku": "작은 모델", "agent-sonnet": "중간 모델",
}

func armName(a string) string {
	if n, ok := armNames[a]; ok {
		return n + " (" + a + ")"
	}
	return a
}

// Markdown renders the report.
func (r Report) Markdown(repo string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s 보정 결과\n\n", repo)
	if r.Tasks == 0 {
		b.WriteString("모든 설정이 끝낸 과제가 없어 비교할 수 없습니다.\n")
		return b.String()
	}
	what := "숨은 테스트 통과"
	if r.Locate {
		what = "실제 수정과 같은 위치를 고침"
	}
	base := r.Arms[0]
	var pick ArmStat
	for _, a := range r.Arms {
		if a.Arm == r.Pick {
			pick = a
		}
	}
	label := "권고"
	if r.Tasks < MinTasks {
		label = "잠정 권고"
		fmt.Fprintf(&b, "> 과제가 %d개뿐입니다. %d개보다 적으면 한두 과제의 우연으로 권고가 바뀌므로, 아래는 참고만 하십시오.\n\n", r.Tasks, MinTasks)
	}
	fmt.Fprintf(&b, "**%s: %s.** ", label, armName(r.Pick))
	if r.Pick == r.Baseline {
		b.WriteString("더 싼 설정은 모두 허용 범위보다 많은 과제를 놓쳤습니다.\n\n")
	} else {
		fmt.Fprintf(&b, "기준보다 문제당 비용이 %.0f%% 낮고($%.3f → $%.3f), 놓친 과제가 허용 범위 안입니다(놓침 %d, 새로 맞힘 %d).\n\n",
			100*(1-pick.Mean/base.Mean), base.Mean, pick.Mean, pick.Lost, pick.Gained)
	}
	fmt.Fprintf(&b, "과제 %d개, 성공 기준: %s. 허용 범위: 기준 대비 순손실 %d개 이하.\n\n", r.Tasks, what, r.Tolerance)
	b.WriteString("| 설정 | 성공 | 문제당 비용 (중앙값) | 성공 1건당 비용 | 턴 | 기준 대비 (놓침 / 새로 맞힘) | 판정 |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, a := range r.Arms {
		per := "–"
		if a.Pass > 0 {
			per = fmt.Sprintf("$%.3f", a.Mean*float64(a.N)/float64(a.Pass))
		}
		vs, verdict := "–", "기준"
		if a.Arm != r.Baseline {
			vs = fmt.Sprintf("%d / %d, p=%.2f", a.Lost, a.Gained, a.P)
			if a.P < 0.01 {
				vs = fmt.Sprintf("%d / %d, p<0.01", a.Lost, a.Gained)
			}
			verdict = "허용 범위 밖"
			if a.Acceptable {
				verdict = "허용 범위 안"
			}
		}
		if a.Arm == r.Pick {
			verdict += ", **권고**"
		}
		fmt.Fprintf(&b, "| %s | %d/%d (%.0f%%) | $%.3f ($%.3f) | %s | %.1f | %s | %s |\n",
			armName(a.Arm), a.Pass, a.N, 100*float64(a.Pass)/float64(a.N), a.Mean, a.Median, per, a.Turns, vs, verdict)
	}
	b.WriteString("\n읽는 법:\n")
	b.WriteString("- 같은 설정을 다시 돌려도 과제 45개 중 3개쯤은 결과가 바뀌었습니다. 그 크기의 차이는 차이로 보지 않는 것이 안전합니다.\n")
	if r.Locate {
		b.WriteString("- 성공은 고친 위치만 봅니다. 수정이 옳은지는 재지 않았습니다.\n")
		if r.Pick != r.Baseline {
			b.WriteString("- 그래서 권고된 설정이 같은 곳을 고치더라도 수정의 질은 기준보다 낮을 수 있습니다. 적용하기 전에 실제 작업 몇 건에서 결과를 직접 확인하십시오.\n")
		}
	}
	if r.Locate {
		b.WriteString("- 과제는 과거의 작은 버그 수정입니다(파일 3개 이하, 60줄 이하). 큰 기능 추가에는 맞지 않을 수 있습니다.\n")
	}
	for _, a := range r.Arms {
		if a.Arm != r.Pick && !a.Acceptable && a.Mean < 0.25*base.Mean && a.Pass > 0 {
			fmt.Fprintf(&b, "- 비용이 우선이면 %s도 선택지입니다. 기준이 성공한 과제의 %.0f%%를 %.0f%%의 비용으로 처리했습니다.\n",
				armName(a.Arm), 100*float64(base.Pass-a.Lost)/float64(max(base.Pass, 1)), 100*a.Mean/base.Mean)
		}
	}
	if r.Errors > 0 {
		fmt.Fprintf(&b, "- 오류로 끝난 실행 %d건은 비교에서 뺐습니다.\n", r.Errors)
	}
	fmt.Fprintf(&b, "\n보정에 든 모델 비용: $%.2f\n", r.Spent)
	return b.String()
}
