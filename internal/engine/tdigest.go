package engine

import (
	"math"
	"sort"
)

// mergingDigest is com.tdunning.math.stats.MergingDigest of t-digest 3.3
// (OpenSearch's TDigestState), with the K_2 scale function, two-level
// compression, alternating merge direction and the size-based merge limit.
// Arithmetic keeps Java's evaluation order and never fuses multiply-adds.
type mergingDigest struct {
	mergeCount        int
	publicCompression float64
	compression       float64
	lastUsedCell      int
	totalWeight       float64
	weight            []float64
	mean              []float64
	unmergedWeight    float64
	tempUsed          int
	tempWeight        []float64
	tempMean          []float64
	order             []int
	min, max          float64
}

func newMergingDigest(compression float64) *mergingDigest {
	if compression < 10 {
		compression = 10
	}
	sizeFudge := 10.0
	if compression < 30 {
		sizeFudge += 20
	}
	size := int(math.Max(float64(2*compression)+sizeFudge, -1))
	bufferSize := 5 * size
	if bufferSize <= 2*size {
		bufferSize = 2 * size
	}
	scale := math.Max(1, float64(bufferSize/size-1))
	public := compression
	internal := float64(math.Sqrt(scale) * public)
	if float64(size) < internal+sizeFudge {
		size = int(math.Ceil(internal + sizeFudge))
	}
	if bufferSize <= 2*size {
		bufferSize = 2 * size
	}
	return &mergingDigest{
		publicCompression: public, compression: internal,
		weight: make([]float64, size), mean: make([]float64, size),
		tempWeight: make([]float64, bufferSize), tempMean: make([]float64, bufferSize), order: make([]int, bufferSize),
		min: math.Inf(1), max: math.Inf(-1),
	}
}

// add is add(double x, int w).
func (td *mergingDigest) add(x float64, w int) {
	if td.tempUsed >= len(td.tempWeight)-td.lastUsedCell-1 {
		td.mergeNewValues(false, td.compression)
	}
	where := td.tempUsed
	td.tempUsed++
	td.tempWeight[where] = float64(w)
	td.tempMean[where] = x
	td.unmergedWeight += float64(w)
	if x < td.min {
		td.min = x
	}
	if x > td.max {
		td.max = x
	}
}

// addDigest is AbstractTDigest.add(TDigest): the centroids of other.
func (td *mergingDigest) addDigest(other *mergingDigest) {
	for _, c := range other.centroids() {
		td.add(c.mean, c.count)
	}
}

func (td *mergingDigest) mergeNewValues(force bool, compression float64) {
	if td.totalWeight == 0 && td.unmergedWeight == 0 {
		return
	}
	if force || td.unmergedWeight > 0 {
		td.merge(td.tempMean, td.tempWeight, td.tempUsed, td.unmergedWeight, td.mergeCount%2 == 1, compression)
		td.mergeCount++
		td.tempUsed = 0
		td.unmergedWeight = 0
	}
}

// k2 scale function
func k2Normalizer(compression, n float64) float64 {
	return compression / (float64(4*math.Log(n/compression)) + 24)
}

func k2Max(q, normalizer float64) float64 {
	return float64(q*(1-q)) / normalizer
}

func (td *mergingDigest) merge(incomingMean, incomingWeight []float64, incomingCount int, unmergedWeight float64, runBackwards bool, compression float64) {
	copy(incomingMean[incomingCount:], td.mean[:td.lastUsedCell])
	copy(incomingWeight[incomingCount:], td.weight[:td.lastUsedCell])
	incomingCount += td.lastUsedCell

	order := td.order[:incomingCount]
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return incomingMean[order[a]] < incomingMean[order[b]] })
	td.totalWeight += unmergedWeight
	if runBackwards {
		for i, j := 0, incomingCount-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	td.lastUsedCell = 0
	td.mean[0] = incomingMean[order[0]]
	td.weight[0] = incomingWeight[order[0]]
	wSoFar := 0.0
	normalizer := k2Normalizer(compression, td.totalWeight)
	for i := 1; i < incomingCount; i++ {
		ix := order[i]
		proposedWeight := td.weight[td.lastUsedCell] + incomingWeight[ix]
		q0 := wSoFar / td.totalWeight
		q2 := (wSoFar + proposedWeight) / td.totalWeight
		addThis := proposedWeight <= float64(td.totalWeight*math.Min(k2Max(q0, normalizer), k2Max(q2, normalizer)))
		if i == 1 || i == incomingCount-1 {
			addThis = false
		}
		if addThis {
			td.weight[td.lastUsedCell] += incomingWeight[ix]
			delta := float64((incomingMean[ix]-td.mean[td.lastUsedCell])*incomingWeight[ix]) / td.weight[td.lastUsedCell]
			td.mean[td.lastUsedCell] = td.mean[td.lastUsedCell] + delta
			incomingWeight[ix] = 0
		} else {
			wSoFar += td.weight[td.lastUsedCell]
			td.lastUsedCell++
			td.mean[td.lastUsedCell] = incomingMean[ix]
			td.weight[td.lastUsedCell] = incomingWeight[ix]
			incomingWeight[ix] = 0
		}
	}
	td.lastUsedCell++
	if runBackwards {
		for i, j := 0, td.lastUsedCell-1; i < j; i, j = i+1, j-1 {
			td.mean[i], td.mean[j] = td.mean[j], td.mean[i]
			td.weight[i], td.weight[j] = td.weight[j], td.weight[i]
		}
	}
	if td.totalWeight > 0 {
		td.min = math.Min(td.min, td.mean[0])
		td.max = math.Max(td.max, td.mean[td.lastUsedCell-1])
	}
}

type tdCentroid struct {
	mean  float64
	count int
}

// centroids is MergingDigest.centroids (which compresses first).
func (td *mergingDigest) centroids() []tdCentroid {
	td.mergeNewValues(true, td.publicCompression)
	out := make([]tdCentroid, td.lastUsedCell)
	for i := range out {
		out[i] = tdCentroid{td.mean[i], int(td.weight[i])}
	}
	return out
}

func (td *mergingDigest) size() int64 {
	return int64(td.totalWeight + td.unmergedWeight)
}

// weightedAverage is AbstractTDigest.weightedAverage.
func weightedAverage(x1, w1, x2, w2 float64) float64 {
	if x1 <= x2 {
		return weightedAverageSorted(x1, w1, x2, w2)
	}
	return weightedAverageSorted(x2, w2, x1, w1)
}

func weightedAverageSorted(x1, w1, x2, w2 float64) float64 {
	x := (float64(x1*w1) + float64(x2*w2)) / (w1 + w2)
	return math.Max(x1, math.Min(x, x2))
}

// quantile is MergingDigest.quantile.
func (td *mergingDigest) quantile(q float64) float64 {
	td.mergeNewValues(false, td.compression)
	if td.lastUsedCell == 0 {
		return math.NaN()
	}
	if td.lastUsedCell == 1 {
		return td.mean[0]
	}
	n := td.lastUsedCell
	weight, mean := td.weight, td.mean
	index := float64(q * td.totalWeight)
	if index < 1 {
		return td.min
	}
	if weight[0] > 1 && index < weight[0]/2 {
		return td.min + float64((index-1)/(weight[0]/2-1)*(mean[0]-td.min))
	}
	if index > td.totalWeight-1 {
		return td.max
	}
	if weight[n-1] > 1 && td.totalWeight-index <= weight[n-1]/2 {
		return td.max - float64((td.totalWeight-index-1)/(weight[n-1]/2-1)*(td.max-mean[n-1]))
	}
	weightSoFar := weight[0] / 2
	for i := 0; i < n-1; i++ {
		dw := (weight[i] + weight[i+1]) / 2
		if weightSoFar+dw > index {
			leftUnit := 0.0
			if weight[i] == 1 {
				if index-weightSoFar < 0.5 {
					return mean[i]
				}
				leftUnit = 0.5
			}
			rightUnit := 0.0
			if weight[i+1] == 1 {
				if weightSoFar+dw-index <= 0.5 {
					return mean[i+1]
				}
				rightUnit = 0.5
			}
			z1 := index - weightSoFar - leftUnit
			z2 := weightSoFar + dw - index - rightUnit
			return weightedAverage(mean[i], z2, mean[i+1], z1)
		}
		weightSoFar += dw
	}
	z1 := index - td.totalWeight - weight[n-1]/2.0
	z2 := weight[n-1]/2 - z1
	return weightedAverage(mean[n-1], z1, td.max, z2)
}

// cdf is MergingDigest.cdf.
func (td *mergingDigest) cdf(x float64) float64 {
	td.mergeNewValues(false, td.compression)
	if td.lastUsedCell == 0 {
		return math.NaN()
	}
	weight, mean := td.weight, td.mean
	if td.lastUsedCell == 1 {
		width := td.max - td.min
		switch {
		case x < td.min:
			return 0
		case x > td.max:
			return 1
		case x-td.min <= width:
			return 0.5
		}
		return (x - td.min) / (td.max - td.min)
	}
	n := td.lastUsedCell
	if x < td.min {
		return 0
	}
	if x > td.max {
		return 1
	}
	if x < mean[0] {
		if mean[0]-td.min > 0 {
			if x == td.min {
				return 0.5 / td.totalWeight
			}
			return (1 + float64((x-td.min)/(mean[0]-td.min)*(weight[0]/2-1))) / td.totalWeight
		}
		return 0
	}
	if x > mean[n-1] {
		if td.max-mean[n-1] > 0 {
			if x == td.max {
				return 1 - 0.5/td.totalWeight
			}
			dq := (1 + float64((td.max-x)/(td.max-mean[n-1])*(weight[n-1]/2-1))) / td.totalWeight
			return 1 - dq
		}
		return 1
	}
	weightSoFar := 0.0
	for it := 0; it < n-1; it++ {
		if mean[it] == x {
			dw := 0.0
			for it < n && mean[it] == x {
				dw += weight[it]
				it++
			}
			return (weightSoFar + dw/2) / td.totalWeight
		} else if mean[it] <= x && x < mean[it+1] {
			if mean[it+1]-mean[it] > 0 {
				leftExcludedW, rightExcludedW := 0.0, 0.0
				if weight[it] == 1 {
					if weight[it+1] == 1 {
						return (weightSoFar + 1) / td.totalWeight
					}
					leftExcludedW = 0.5
				} else if weight[it+1] == 1 {
					rightExcludedW = 0.5
				}
				dw := (weight[it] + weight[it+1]) / 2
				left, right := mean[it], mean[it+1]
				dwNoSingleton := dw - leftExcludedW - rightExcludedW
				base := weightSoFar + weight[it]/2 + leftExcludedW
				return (base + float64(dwNoSingleton*(x-left))/(right-left)) / td.totalWeight
			}
			dw := (weight[it] + weight[it+1]) / 2
			return (weightSoFar + dw) / td.totalWeight
		} else {
			weightSoFar += weight[it]
		}
	}
	if x == mean[n-1] {
		return 1 - 0.5/td.totalWeight
	}
	return math.NaN()
}
