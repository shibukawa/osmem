package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestTDigestOracle compares the t-digest port with the Java library when
// TDIGEST_ORACLE (a directory holding TD.class and the jars) is set.
func TestTDigestOracle(t *testing.T) {
	dir := os.Getenv("TDIGEST_ORACLE")
	if dir == "" {
		t.Skip("TDIGEST_ORACLE not set")
	}
	rng := rand.New(rand.NewSource(42))
	var in bytes.Buffer
	var cases []string
	var want []func() string
	n := 3000
	if s := os.Getenv("TDIGEST_CASES"); s != "" {
		fmt.Sscan(s, &n)
	}
	for c := 0; c < n; c++ {
		size := 1 + rng.Intn(40)
		switch rng.Intn(6) {
		case 0:
			size = 1 + rng.Intn(3000)
		case 1:
			size = 1 + rng.Intn(400)
		}
		vals := make([]float64, size)
		distinct := 1 + rng.Intn(size+1)
		for i := range vals {
			switch c % 4 {
			case 0:
				vals[i] = float64(rng.Intn(distinct))
			case 1:
				vals[i] = math.Round(rng.NormFloat64()*1000) / 100
			case 2:
				vals[i] = rng.ExpFloat64() * 50
			default:
				vals[i] = float64(rng.Intn(5))
			}
		}
		comp := []float64{100, 100, 20, 1000, 5, 250}[rng.Intn(6)]
		ps := []float64{0, 0.1, 1, 5, 10, 25, 33.3, 50, 66, 75, 90, 95, 99, 99.9, 100}
		xs := []float64{-1, 0, 1, 2.5, 10, 50, 100}
		vs := make([]string, len(vals))
		for i, v := range vals {
			vs[i] = javaNumberString(v, 64)
		}
		fmtList := func(list []float64) string {
			parts := make([]string, len(list))
			for i, v := range list {
				parts[i] = javaNumberString(v, 64)
			}
			return fmt.Sprintf("%d %s", len(list), strings.Join(parts, " "))
		}
		valsCopy := append([]float64(nil), vals...)
		mode := []string{"pct", "rank", "mad"}[c%3]
		switch mode {
		case "pct":
			fmt.Fprintf(&in, "pct %s %d %s %s\n", javaNumberString(comp, 64), len(vals), strings.Join(vs, " "), fmtList(ps))
			want = append(want, func() string {
				shard := newMergingDigest(comp)
				for _, v := range valsCopy {
					shard.add(v, 1)
				}
				red := newMergingDigest(comp)
				red.addDigest(shard)
				parts := make([]string, len(ps))
				for i, p := range ps {
					parts[i] = javaDoubleToString(red.quantile(p / 100))
				}
				return strings.Join(parts, " ")
			})
		case "rank":
			fmt.Fprintf(&in, "rank %s %d %s %s\n", javaNumberString(comp, 64), len(vals), strings.Join(vs, " "), fmtList(xs))
			want = append(want, func() string {
				shard := newMergingDigest(comp)
				for _, v := range valsCopy {
					shard.add(v, 1)
				}
				red := newMergingDigest(comp)
				red.addDigest(shard)
				parts := make([]string, len(xs))
				for i, x := range xs {
					parts[i] = javaDoubleToString(float64(red.cdf(x) * 100))
				}
				return strings.Join(parts, " ")
			})
		case "mad":
			fmt.Fprintf(&in, "mad %s %d %s\n", javaNumberString(comp, 64), len(vals), strings.Join(vs, " "))
			want = append(want, func() string {
				shard := newMergingDigest(comp)
				for _, v := range valsCopy {
					shard.add(v, 1)
				}
				red := newMergingDigest(comp)
				red.addDigest(shard)
				return javaDoubleToString(medianAbsoluteDeviation(red, comp))
			})
		}
		cases = append(cases, fmt.Sprintf("case %d mode %s comp %v n %d", c, mode, comp, len(vals)))
	}
	cmd := exec.Command("java", "-cp", dir+"/../jars/t-digest-3.3.jar:"+dir+"/../jars/HdrHistogram-2.2.2.jar:"+dir, "TD")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	i, bad := 0, 0
	for sc.Scan() {
		got := want[i]()
		if !sameDoubles(got, sc.Text(), 1) {
			bad++
			if bad <= 10 {
				t.Errorf("%s:\n java %s\n go   %s", cases[i], sc.Text(), got)
			}
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("java answered %d of %d cases", i, len(want))
	}
	if bad > 0 {
		t.Errorf("%d of %d cases differ", bad, len(want))
	}
}

// sameDoubles compares space-separated numbers by value: older JDKs print
// some doubles with more digits than the shortest representation.
func sameDoubles(a, b string, ulps int) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	if len(fa) != len(fb) {
		return false
	}
	for i := range fa {
		x, err1 := strconv.ParseFloat(fa[i], 64)
		y, err2 := strconv.ParseFloat(fb[i], 64)
		if err1 != nil || err2 != nil {
			if fa[i] != fb[i] {
				return false
			}
			continue
		}
		if x == y || math.IsNaN(x) && math.IsNaN(y) {
			continue
		}
		// last-bit differences remain on some merged digests
		if ulps > 0 && math.Abs(x-y) <= 1e-12*math.Max(math.Abs(x), math.Abs(y)) {
			continue
		}
		return false
	}
	return true
}
