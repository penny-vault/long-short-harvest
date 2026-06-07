// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"errors"
	"fmt"
	"math"

	"github.com/sjwhitworth/golearn/base"
	"github.com/sjwhitworth/golearn/ensemble"
)

// regimeFeatureCount is the dimensionality of the feature vector consumed by
// the random-forest regime classifier. Mirrors the QC implementation.
const regimeFeatureCount = 11

// regimeModel wraps a golearn RandomForest classifier with a per-feature
// standard scaler. It is trained on (VIX,SPY) feature vectors with binary
// labels; predictions return P(label=1) i.e. P(SPY 21d forward return > 2%).
type regimeModel struct {
	rf      *ensemble.RandomForest
	mean    [regimeFeatureCount]float64
	std     [regimeFeatureCount]float64
	trained bool
	// singleClass records the fixed class label when the training set
	// contains only one label. QC's CheckSignal_Long handles this case via
	// the predict_proba fallback `prob = 0.7 if predict==1 else 0.5`,
	// which means ml_bullish=True (since 0.7 > 0.6 threshold) whenever the
	// training set is all-bullish. golearn's Predict returns just the
	// class index, so we replicate the fallback explicitly. Values: -1 if
	// mixed labels (use rf.Predict), 0 if all-bearish, 1 if all-bullish.
	singleClass int
	// Diagnostics captured at training time for cross-comparison with the
	// QC reference's DBG_TRAIN log lines.
	trainVixLen      int
	trainSpyLen      int
	trainSamples     int
	trainZeros       int
	trainOnes        int
	trainLabels      []int      // full label sequence; lets us diff the exact y array
	trainFirstRow    []float64  // 11-feature first sample
	trainFirstLabel  int
	trainLastRow     []float64
	trainLastLabel   int
}

// regimeFeatures computes the 11-feature vector consumed by the random-forest
// regime classifier from trailing VIX and SPY close series. Feature order
// matches the QC GetFeatures function.
func regimeFeatures(vixCloses, spyCloses []float64) ([regimeFeatureCount]float64, bool) {
	var f [regimeFeatureCount]float64
	if len(vixCloses) < 50 || len(spyCloses) < 200 {
		return f, false
	}

	currentVix := vixCloses[len(vixCloses)-1]
	vixSMA20 := mean(vixCloses[len(vixCloses)-20:])
	vixSMA50 := mean(vixCloses[len(vixCloses)-50:])
	vixSD := stdDev(vixCloses[len(vixCloses)-20:])

	zscore := 0.0
	if !math.IsNaN(vixSD) && vixSD > 0 {
		zscore = (currentVix - vixSMA20) / vixSD
	}

	pct := percentileRank(vixCloses, currentVix)
	if math.IsNaN(pct) {
		return f, false
	}

	spyNow := spyCloses[len(spyCloses)-1]
	spySMA50 := mean(spyCloses[len(spyCloses)-50:])
	spySMA200 := mean(spyCloses[len(spyCloses)-200:])

	if math.IsNaN(spyNow) || math.IsNaN(spySMA50) || math.IsNaN(spySMA200) {
		return f, false
	}

	// QC's GetFeatures uses spy_closes[-1]/spy_closes[-N], which in Go is
	// returnNAgo(closes, N-1) since that function looks back N+1 elements
	// for an N-bar return. The names "5d/10d/20d" in QC are off-by-one --
	// they're really 4/9/19-bar returns. Match exactly.
	spy5 := returnNAgo(spyCloses, 4)
	spy10 := returnNAgo(spyCloses, 9)
	spy20 := returnNAgo(spyCloses, 19)

	dailyReturns := pctReturns(spyCloses[len(spyCloses)-21:])
	spyVol := stdDev(dailyReturns)

	f[0] = currentVix
	f[1] = zscore
	f[2] = pct
	if vixSMA20 != 0 {
		f[3] = currentVix / vixSMA20
	} else {
		f[3] = 1.0
	}
	if vixSMA50 != 0 {
		f[4] = currentVix / vixSMA50
	} else {
		f[4] = 1.0
	}
	f[5] = spy5
	f[6] = spy10
	f[7] = spy20
	if spySMA50 != 0 {
		f[8] = spyNow / spySMA50
	} else {
		f[8] = 1.0
	}
	if spySMA200 != 0 {
		f[9] = spyNow / spySMA200
	} else {
		f[9] = 1.0
	}
	if !math.IsNaN(spyVol) {
		f[10] = spyVol * math.Sqrt(252)
	}

	for _, v := range f {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return f, false
		}
	}
	return f, true
}

// returnNAgo returns the simple return between the last close and n bars ago.
func returnNAgo(closes []float64, n int) float64 {
	if len(closes) <= n {
		return math.NaN()
	}
	prev := closes[len(closes)-1-n]
	last := closes[len(closes)-1]
	if math.IsNaN(prev) || math.IsNaN(last) || prev == 0 {
		return math.NaN()
	}
	return last/prev - 1.0
}

// trainRegimeModel constructs labelled examples from the supplied series and
// fits a RandomForest classifier. The training procedure mirrors the QC
// VolatilityHarvestML_LongShort.TrainModel method: walk forward from index
// 200 to len-21, build a feature vector from the prefix, label 1 if the SPY
// 21-day forward return exceeds 2%.
func trainRegimeModel(vixCloses, spyCloses []float64, minSamples int) (*regimeModel, error) {
	if len(vixCloses) < minSamples || len(spyCloses) < minSamples {
		return nil, errors.New("trainRegimeModel: insufficient history")
	}
	if len(spyCloses) <= 221 {
		return nil, errors.New("trainRegimeModel: need at least 222 SPY observations")
	}

	rows := make([][regimeFeatureCount]float64, 0, len(spyCloses))
	labels := make([]int, 0, len(spyCloses))

	for i := 200; i < len(spyCloses)-21; i++ {
		// QC's training loop calls GetFeatures(vix_closes[:i], spy_closes[:i]).
		// In Python, slice [:i] beyond array length is equivalent to [:len].
		// QC's vix_closes is shorter than spy_closes (~549 vs 800), so for
		// i > len(vix), vix_closes[:i] caps at len(vix). Replicate that here:
		// Go's [:i] would panic past array length, so clamp the upper bound.
		vixEnd := i
		if vixEnd > len(vixCloses) {
			vixEnd = len(vixCloses)
		}
		vixPrefix := vixCloses[:vixEnd]
		spyPrefix := spyCloses[:i]
		feats, ok := regimeFeatures(vixPrefix, spyPrefix)
		if !ok {
			continue
		}

		// QC's TrainModel uses `label = 1 if spy_closes[i+21]/spy_closes[i] > 0.02 else 0`.
		// The comparison is on the raw ratio, not the percentage return,
		// so for any positive SPY prices the result is always True. The
		// training set is therefore single-class (all 1s), which makes
		// sklearn's RandomForest emit predict_proba of length 1, and QC's
		// CheckSignal_Long falls back to prob=0.7 (-> ml_bullish=True
		// always after the first training day). Replicate exactly.
		fwd := spyCloses[i+21] / spyCloses[i]
		label := 0
		if fwd > 0.02 {
			label = 1
		}
		rows = append(rows, feats)
		labels = append(labels, label)
	}

	if len(rows) < 100 {
		return nil, fmt.Errorf("trainRegimeModel: need at least 100 training rows, got %d", len(rows))
	}

	// Detect single-class training set so predictBullish can fall back to
	// the QC-equivalent prob=0.7 path instead of hitting golearn's
	// rf.Predict, which on a single-class fit can return inconsistent
	// outputs.
	singleClass := -1
	zeros, ones := 0, 0
	for _, lbl := range labels {
		if lbl == 0 {
			zeros++
		} else {
			ones++
		}
	}
	if zeros == 0 && ones > 0 {
		singleClass = 1
	} else if ones == 0 && zeros > 0 {
		singleClass = 0
	}

	model := &regimeModel{
		singleClass:     singleClass,
		trainVixLen:     len(vixCloses),
		trainSpyLen:     len(spyCloses),
		trainSamples:    len(rows),
		trainZeros:      zeros,
		trainOnes:       ones,
		trainLabels:     append([]int(nil), labels...),
		trainFirstRow:   append([]float64(nil), rows[0][:]...),
		trainFirstLabel: labels[0],
		trainLastRow:    append([]float64(nil), rows[len(rows)-1][:]...),
		trainLastLabel:  labels[len(labels)-1],
	}
	for j := 0; j < regimeFeatureCount; j++ {
		col := make([]float64, len(rows))
		for i := range rows {
			col[i] = rows[i][j]
		}
		model.mean[j] = mean(col)
		s := stdDev(col)
		if math.IsNaN(s) || s == 0 {
			s = 1
		}
		model.std[j] = s
	}

	instances := buildInstances(rows, labels, model)

	rf := ensemble.NewRandomForest(100, 4)
	if err := rf.Fit(instances); err != nil {
		return nil, fmt.Errorf("trainRegimeModel: fit: %w", err)
	}

	model.rf = rf
	model.trained = true
	if singleClass >= 0 {
		// Skip rf.Fit'd predictions; the QC fallback short-circuits these
		// cases. We still keep rf attached for completeness.
		_ = rf
	}
	return model, nil
}

// predictBullish returns true when the model's predicted P(label=1) for the
// supplied features exceeds 0.6. Returns false when the model is untrained or
// the prediction fails.
//
// QC's predict_proba fallback path applies when the training set was
// single-class: prob = 0.7 if predict==1 else 0.5, threshold 0.6. So a
// single-class-bullish training set yields ml_bullish=True every day; a
// single-class-bearish set yields False. golearn's Predict returns the class
// index without a probability, so we replicate the fallback directly.
func (m *regimeModel) predictBullish(features [regimeFeatureCount]float64) bool {
	if m == nil || !m.trained {
		return false
	}

	if m.singleClass >= 0 {
		// QC: prob = 0.7 if predict==1 else 0.5; ml_bullish = prob > 0.6.
		return m.singleClass == 1
	}

	scaled := [regimeFeatureCount]float64{}
	for j := 0; j < regimeFeatureCount; j++ {
		scaled[j] = (features[j] - m.mean[j]) / m.std[j]
	}

	instances := buildPredictionInstances(scaled)
	predicted, err := m.rf.Predict(instances)
	if err != nil {
		return false
	}

	row := predicted.RowString(0)
	return row == "1"
}

// buildInstances assembles a golearn DenseInstances frame from scaled feature
// rows and class labels.
func buildInstances(rows [][regimeFeatureCount]float64, labels []int, model *regimeModel) base.FixedDataGrid {
	attrs := make([]base.Attribute, regimeFeatureCount+1)
	for j := 0; j < regimeFeatureCount; j++ {
		attrs[j] = base.NewFloatAttribute(fmt.Sprintf("f%d", j))
	}
	classAttr := base.NewCategoricalAttribute()
	classAttr.SetName("class")
	_ = classAttr.GetSysValFromString("0")
	_ = classAttr.GetSysValFromString("1")
	attrs[regimeFeatureCount] = classAttr

	instances := base.NewDenseInstances()
	specs := make([]base.AttributeSpec, len(attrs))
	for i, a := range attrs {
		specs[i] = instances.AddAttribute(a)
	}
	if err := instances.AddClassAttribute(attrs[regimeFeatureCount]); err != nil {
		return nil
	}
	if err := instances.Extend(len(rows)); err != nil {
		return nil
	}

	for i := range rows {
		for j := 0; j < regimeFeatureCount; j++ {
			scaled := (rows[i][j] - model.mean[j]) / model.std[j]
			instances.Set(specs[j], i, base.PackFloatToBytes(scaled))
		}
		instances.Set(specs[regimeFeatureCount], i, classAttr.GetSysValFromString(fmt.Sprintf("%d", labels[i])))
	}

	return instances
}

// buildPredictionInstances creates a single-row DenseInstances frame for
// prediction. The class attribute is left unset.
func buildPredictionInstances(features [regimeFeatureCount]float64) base.FixedDataGrid {
	attrs := make([]base.Attribute, regimeFeatureCount+1)
	for j := 0; j < regimeFeatureCount; j++ {
		attrs[j] = base.NewFloatAttribute(fmt.Sprintf("f%d", j))
	}
	classAttr := base.NewCategoricalAttribute()
	classAttr.SetName("class")
	_ = classAttr.GetSysValFromString("0")
	_ = classAttr.GetSysValFromString("1")
	attrs[regimeFeatureCount] = classAttr

	instances := base.NewDenseInstances()
	specs := make([]base.AttributeSpec, len(attrs))
	for i, a := range attrs {
		specs[i] = instances.AddAttribute(a)
	}
	if err := instances.AddClassAttribute(attrs[regimeFeatureCount]); err != nil {
		return nil
	}
	if err := instances.Extend(1); err != nil {
		return nil
	}

	for j := 0; j < regimeFeatureCount; j++ {
		instances.Set(specs[j], 0, base.PackFloatToBytes(features[j]))
	}

	return instances
}
