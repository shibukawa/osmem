package engine

import "time"

// aggTypes are the aggregation types osmem knows.
var aggTypes map[string]*aggType

const (
	pkgMetrics  = "org.opensearch.search.aggregations.metrics."
	pkgBucket   = "org.opensearch.search.aggregations.bucket."
	pkgPipeline = "org.opensearch.search.aggregations.pipeline."
)

func init() {
	metric := func(class string, kind int) *aggType {
		return &aggType{class: pkgMetrics + class, leaf: true, metric: kind, parse: parseMetric, prepare: prepareMetric, collect: collectMetric}
	}
	aggTypes = map[string]*aggType{
		"terms": {class: pkgBucket + "terms.TermsAggregationBuilder", card: cardMany,
			parse: parseTermsAgg, prepare: prepareTerms, collect: collectTerms},
		"multi_terms": {class: pkgBucket + "terms.MultiTermsAggregationBuilder", card: cardMany,
			parse: parseMultiTerms, prepare: prepareMultiTerms, collect: collectMultiTerms},
		"rare_terms": {class: pkgBucket + "terms.RareTermsAggregationBuilder", card: cardMany,
			parse: parseRareTerms, prepare: prepareRareTerms, collect: collectRareTerms},
		"significant_terms": {class: pkgBucket + "terms.SignificantTermsAggregationBuilder", card: cardMany,
			parse: parseSignificantTerms, prepare: prepareSignificantTerms, collect: collectSignificant},
		"significant_text": {class: pkgBucket + "terms.SignificantTextAggregationBuilder", card: cardMany,
			parse: parseSignificantText, prepare: prepareSignificantText, collect: collectSignificant},

		"avg":         metric("AvgAggregationBuilder", metricSingle),
		"sum":         metric("SumAggregationBuilder", metricSingle),
		"min":         metric("MinAggregationBuilder", metricSingle),
		"max":         metric("MaxAggregationBuilder", metricSingle),
		"value_count": metric("ValueCountAggregationBuilder", metricSingle),
		"cardinality": metric("CardinalityAggregationBuilder", metricSingle),
		"stats": {class: pkgMetrics + "StatsAggregationBuilder", leaf: true, metric: metricMulti, parse: parseMetric, prepare: prepareMetric, collect: collectMetric,
			hasMetric: func(d *aggDef, name string) bool { return statsNames[name] }},
		"extended_stats": {class: pkgMetrics + "ExtendedStatsAggregationBuilder", leaf: true, metric: metricMulti, parse: parseMetric, prepare: prepareMetric, collect: collectMetric,
			hasMetric: func(d *aggDef, name string) bool { return extStatsNames[name] }},
		"weighted_avg": {class: pkgMetrics + "WeightedAvgAggregationBuilder", leaf: true, metric: metricSingle,
			parse: parseWeightedAvg, prepare: prepareWeightedAvg, collect: collectWeightedAvg},
		"top_hits": {class: pkgMetrics + "TopHitsAggregationBuilder", leaf: true,
			parse: parseTopHits, prepare: prepareTopHits, collect: collectTopHits},
		"percentiles": {class: pkgMetrics + "PercentilesAggregationBuilder", leaf: true, metric: metricMulti,
			parse: parsePercentiles, prepare: preparePercentiles, collect: collectPercentiles, hasMetric: percentilesHasMetric},
		"percentile_ranks": {class: pkgMetrics + "PercentileRanksAggregationBuilder", leaf: true, metric: metricMulti,
			parse: parsePercentiles, prepare: preparePercentiles, collect: collectPercentiles, hasMetric: percentilesHasMetric},
		"median_absolute_deviation": {class: pkgMetrics + "MedianAbsoluteDeviationAggregationBuilder", leaf: true, metric: metricSingle,
			parse: parsePercentiles, prepare: preparePercentiles, collect: collectPercentiles},
		"geo_bounds": {class: "org.opensearch.geo.search.aggregations.metrics.GeoBoundsAggregationBuilder", leaf: true,
			parse: parseGeoMetric, prepare: prepareGeoMetric, collect: collectGeoBounds},
		"geo_centroid": {class: pkgMetrics + "GeoCentroidAggregationBuilder", leaf: true,
			parse: parseGeoMetric, prepare: prepareGeoMetric, collect: collectGeoCentroid},
		"geohash_grid": {class: "org.opensearch.geo.search.aggregations.bucket.geogrid.GeoHashGridAggregationBuilder", card: cardMany,
			parse: parseGeoGrid, prepare: prepareGeoGrid, collect: collectGeoGrid},
		"geotile_grid": {class: "org.opensearch.geo.search.aggregations.bucket.geogrid.GeoTileGridAggregationBuilder", card: cardMany,
			parse: parseGeoGrid, prepare: prepareGeoGrid, collect: collectGeoGrid},

		"histogram": {class: pkgBucket + "histogram.HistogramAggregationBuilder", card: cardMany,
			parse: parseHistogram, prepare: prepareHistogram, collect: collectHistogram},
		"date_histogram": {class: pkgBucket + "histogram.DateHistogramAggregationBuilder", card: cardMany,
			parse: parseDateHistogram, prepare: prepareDateHistogram, collect: collectDateHistogram},
		"auto_date_histogram": {class: pkgBucket + "histogram.AutoDateHistogramAggregationBuilder", card: cardMany,
			parse: parseAutoDateHistogram, prepare: prepareAutoDateHistogram, collect: collectAutoDateHistogram},
		"variable_width_histogram": {class: pkgBucket + "histogram.VariableWidthHistogramAggregationBuilder", card: cardMany,
			parse: parseVariableWidthHistogram, prepare: prepareVariableWidthHistogram, collect: collectVariableWidthHistogram},

		"range": {class: pkgBucket + "range.RangeAggregationBuilder", card: cardMany,
			parse: parseRangeAgg, prepare: prepareRange, collect: collectRange},
		"date_range": {class: pkgBucket + "range.DateRangeAggregationBuilder", card: cardMany,
			parse: parseRangeAgg, prepare: prepareRange, collect: collectRange},
		"ip_range": {class: pkgBucket + "range.IpRangeAggregationBuilder", card: cardMany,
			parse: parseRangeAgg, prepare: prepareRange, collect: collectRange},
		"geo_distance": {class: pkgBucket + "range.GeoDistanceAggregationBuilder", card: cardMany,
			parse: parseRangeAgg, prepare: prepareRange, collect: collectRange},
		"composite": {class: pkgBucket + "composite.CompositeAggregationBuilder", card: cardOne,
			parse: parseComposite, prepare: prepareComposite, collect: collectComposite},

		"avg_bucket":            siblingPipeline("AvgBucketPipelineAggregationBuilder"),
		"sum_bucket":            siblingPipeline("SumBucketPipelineAggregationBuilder"),
		"min_bucket":            siblingPipeline("MinBucketPipelineAggregationBuilder"),
		"max_bucket":            siblingPipeline("MaxBucketPipelineAggregationBuilder"),
		"stats_bucket":          siblingPipeline("StatsBucketPipelineAggregationBuilder"),
		"extended_stats_bucket": siblingPipeline("ExtendedStatsBucketPipelineAggregationBuilder"),
		"percentiles_bucket":    siblingPipeline("PercentilesBucketPipelineAggregationBuilder"),
		"cumulative_sum":        parentPipeline("CumulativeSumPipelineAggregationBuilder"),
		"derivative":            parentPipeline("DerivativePipelineAggregationBuilder"),
		"serial_diff":           parentPipeline("SerialDiffPipelineAggregationBuilder"),
		"moving_avg":            parentPipeline("MovAvgPipelineAggregationBuilder"),
		"bucket_sort": {class: pkgPipeline + "BucketSortPipelineAggregationBuilder", pipeline: true,
			parse: parseBucketSort, parent: applyParentPipeline, validate: validateBucketSort, paths: pipelinePaths},

		"filter": {class: pkgBucket + "filter.FilterAggregationBuilder", card: cardOne,
			parse: parseFilter, collect: collectFilter},
		"filters": {class: pkgBucket + "filter.FiltersAggregationBuilder", card: cardMany,
			parse: parseFilters, collect: collectFilters},
		"adjacency_matrix": {class: pkgBucket + "adjacency.AdjacencyMatrixAggregationBuilder", card: cardMany,
			parse: parseAdjacency, collect: collectAdjacency},
		"missing": {class: pkgBucket + "missing.MissingAggregationBuilder", card: cardOne,
			parse: parseMissingAgg, prepare: prepareMissingAgg, collect: collectMissingAgg},
		"global": {class: pkgBucket + "global.GlobalAggregationBuilder", card: cardOne,
			parse: parseGlobal, prepare: prepareGlobal, collect: collectGlobal},
		"nested": {class: pkgBucket + "nested.NestedAggregationBuilder", card: cardOne,
			parse: parseNestedAgg, prepare: prepareNestedAgg, collect: collectNested},
		"reverse_nested": {class: pkgBucket + "nested.ReverseNestedAggregationBuilder", card: cardOne,
			parse: parseNestedAgg, prepare: prepareNestedAgg, collect: collectReverseNested},
		"sampler": {class: pkgBucket + "sampler.SamplerAggregationBuilder", card: cardOne,
			parse: parseSampler, prepare: prepareSampler, collect: collectSampler},
		"diversified_sampler": {class: pkgBucket + "sampler.DiversifiedAggregationBuilder", card: cardOne,
			parse: parseDiversified, prepare: prepareSampler, collect: collectSampler},
		"children": {class: "org.opensearch.join.aggregations.ChildrenAggregationBuilder", card: cardOne,
			parse: parseJoinAgg, prepare: prepareJoinAgg, collect: collectJoinAgg},
		"parent": {class: "org.opensearch.join.aggregations.ParentAggregationBuilder", card: cardOne,
			parse: parseJoinAgg, prepare: prepareJoinAgg, collect: collectJoinAgg},
	}
	for name, class := range pendingAggTypes {
		if _, ok := aggTypes[name]; !ok {
			aggTypes[name] = &aggType{class: class, parse: parseUnsupported}
		}
	}
}

func siblingPipeline(class string) *aggType {
	return &aggType{class: pkgPipeline + class, pipeline: true, parse: parseBucketMetrics, sibling: siblingBucketMetrics,
		validate: validateBucketMetrics, paths: pipelinePaths}
}

func parentPipeline(class string) *aggType {
	return &aggType{class: pkgPipeline + class, pipeline: true, parse: parseParentPipeline, parent: applyParentPipeline,
		validate: validateParentPipeline, paths: pipelinePaths}
}

// pendingAggTypes are registered OpenSearch aggregations osmem rejects.
var pendingAggTypes = map[string]string{
	"rare_terms":               pkgBucket + "terms.RareTermsAggregationBuilder",
	"significant_terms":        pkgBucket + "terms.SignificantTermsAggregationBuilder",
	"significant_text":         pkgBucket + "terms.SignificantTextAggregationBuilder",
	"auto_date_histogram":      pkgBucket + "histogram.AutoDateHistogramAggregationBuilder",
	"variable_width_histogram": pkgBucket + "histogram.VariableWidthHistogramAggregationBuilder",
	"matrix_stats":             "org.opensearch.search.aggregations.matrix.stats.MatrixStatsAggregationBuilder",
	"scripted_metric":          pkgMetrics + "ScriptedMetricAggregationBuilder",
	"bucket_script":            pkgPipeline + "BucketScriptPipelineAggregationBuilder",
	"bucket_selector":          pkgPipeline + "BucketSelectorPipelineAggregationBuilder",
	"moving_fn":                pkgPipeline + "MovFnPipelineAggregationBuilder",
	"geohex_grid":              "org.opensearch.geospatial.search.aggregations.bucket.geogrid.GeoHexGridAggregationBuilder",
}

func parseUnsupported(ps *aggParser, d *aggDef) error {
	return errUnsupported("[" + d.kind + "] aggregation")
}

func timeNow() time.Time { return time.Now() }
