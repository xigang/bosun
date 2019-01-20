package graphite

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"bosun.org/cmd/bosun/expr"
	"bosun.org/cmd/bosun/expr/parse"
	"bosun.org/graphite"
	"bosun.org/models"
	"bosun.org/opentsdb"
	"github.com/MiniProfiler/go/miniprofiler"
)

// ExprFuncs defines functions for use with a Graphite backend.
var ExprFuncs = map[string]parse.Func{
	"graphiteBand": {
		Args:    []models.FuncType{models.TypeString, models.TypeString, models.TypeString, models.TypeString, models.TypeScalar},
		Return:  models.TypeSeriesSet,
		TagKeys: graphiteTagQuery,
		F:       Band,
	},
	"graphite": {
		Args:    []models.FuncType{models.TypeString, models.TypeString, models.TypeString, models.TypeString},
		Return:  models.TypeSeriesSet,
		TagKeys: graphiteTagQuery,
		F:       Query,
	},
}

func parseGraphiteResponse(req *graphite.Request, s *graphite.Response, formatTags []string) ([]*expr.Element, error) {
	const parseErrFmt = "graphite ParseError (%s): %s"
	if len(*s) == 0 {
		return nil, fmt.Errorf(parseErrFmt, req.URL, "empty response")
	}
	seen := make(map[string]bool)
	results := make([]*expr.Element, 0)
	for _, res := range *s {
		// build tag set
		tags := make(opentsdb.TagSet)
		if len(formatTags) == 1 && formatTags[0] == "" {
			tags["key"] = res.Target
		} else {
			nodes := strings.Split(res.Target, ".")
			if len(nodes) < len(formatTags) {
				msg := fmt.Sprintf("returned target '%s' does not match format '%s'", res.Target, strings.Join(formatTags, ","))
				return nil, fmt.Errorf(parseErrFmt, req.URL, msg)
			}
			for i, key := range formatTags {
				if len(key) > 0 {
					tags[key] = nodes[i]
				}
			}
		}
		if !tags.Valid() {
			msg := fmt.Sprintf("returned target '%s' would make an invalid tag '%s'", res.Target, tags.String())
			return nil, fmt.Errorf(parseErrFmt, req.URL, msg)
		}
		if ts := tags.String(); !seen[ts] {
			seen[ts] = true
		} else {
			return nil, fmt.Errorf(parseErrFmt, req.URL, fmt.Sprintf("More than 1 series identified by tagset '%v'", ts))
		}
		// build data
		dps := make(expr.Series)
		for _, dp := range res.Datapoints {
			if len(dp) != 2 {
				return nil, fmt.Errorf(parseErrFmt, req.URL, fmt.Sprintf("Datapoint has != 2 fields: %v", dp))
			}
			if len(dp[0].String()) == 0 {
				// none value. skip this record
				continue
			}
			val, err := dp[0].Float64()
			if err != nil {
				msg := fmt.Sprintf("value '%s' cannot be decoded to Float64: %s", dp[0], err.Error())
				return nil, fmt.Errorf(parseErrFmt, req.URL, msg)
			}
			unixTS, err := dp[1].Int64()
			if err != nil {
				msg := fmt.Sprintf("timestamp '%s' cannot be decoded to Int64: %s", dp[1], err.Error())
				return nil, fmt.Errorf(parseErrFmt, req.URL, msg)
			}
			t := time.Unix(unixTS, 0)
			dps[t] = val
		}
		results = append(results, &expr.Element{
			Value: dps,
			Group: tags,
		})
	}
	return results, nil
}

// Band maps to the "graphiteBand" function in Bosun's expression language.
func Band(e *expr.State, query, duration, period, format string, num float64) (r *expr.ValueSet, err error) {
	r = new(expr.ValueSet)
	r.IgnoreOtherUnjoined = true
	r.IgnoreUnjoined = true
	e.Timer.Step("graphiteBand", func(T miniprofiler.Timer) {
		var d, p opentsdb.Duration
		d, err = opentsdb.ParseDuration(duration)
		if err != nil {
			return
		}
		p, err = opentsdb.ParseDuration(period)
		if err != nil {
			return
		}
		if num < 1 || num > 100 {
			err = fmt.Errorf("expr: Band: num out of bounds")
		}
		req := &graphite.Request{
			Targets: []string{query},
		}
		now := e.Now()
		req.End = &now
		st := now.Add(-time.Duration(d))
		req.Start = &st
		for i := 0; i < int(num); i++ {
			now = now.Add(time.Duration(-p))
			req.End = &now
			st := now.Add(time.Duration(-d))
			req.Start = &st
			var s graphite.Response
			s, err = timeRequest(e, req)
			if err != nil {
				return
			}
			formatTags := strings.Split(format, ".")
			var results []*expr.Element
			results, err = parseGraphiteResponse(req, &s, formatTags)
			if err != nil {
				return
			}
			if i == 0 {
				r.Elements = results
			} else {
				// different graphite requests might return series with different id's.
				// i.e. a different set of tagsets.  merge the data of corresponding tagsets
				for _, result := range results {
					updateKey := -1
					for j, existing := range r.Elements {
						if result.Group.Equal(existing.Group) {
							updateKey = j
							break
						}
					}
					if updateKey == -1 {
						// result tagset is new
						r.Append(result)
						updateKey = len(r.Elements) - 1
					}
					for k, v := range result.Value.(expr.Series) {
						r.Elements[updateKey].Value.(expr.Series)[k] = v
					}
				}
			}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("graphiteBand: %v", err)
	}
	return
}

// Query maps to the "graphite" function in Bosun's expression language.
func Query(e *expr.State, query string, sduration, eduration, format string) (r *expr.ValueSet, err error) {
	sd, err := opentsdb.ParseDuration(sduration)
	if err != nil {
		return
	}
	ed := opentsdb.Duration(0)
	if eduration != "" {
		ed, err = opentsdb.ParseDuration(eduration)
		if err != nil {
			return
		}
	}
	st := e.Now().Add(-time.Duration(sd))
	et := e.Now().Add(-time.Duration(ed))
	req := &graphite.Request{
		Targets: []string{query},
		Start:   &st,
		End:     &et,
	}
	s, err := timeRequest(e, req)
	if err != nil {
		return nil, err
	}
	formatTags := strings.Split(format, ".")
	r = new(expr.ValueSet)
	results, err := parseGraphiteResponse(req, &s, formatTags)
	if err != nil {
		return nil, err
	}
	r.Elements = results

	return
}

func graphiteTagQuery(args []parse.Node) (parse.TagKeys, error) {
	t := make(parse.TagKeys)
	n := args[3].(*parse.StringNode)
	for _, s := range strings.Split(n.Text, ".") {
		if s != "" {
			t[s] = struct{}{}
		}
	}
	return t, nil
}

func timeRequest(e *expr.State, req *graphite.Request) (resp graphite.Response, err error) {
	e.GraphiteQueries = append(e.GraphiteQueries, *req)
	b, _ := json.MarshalIndent(req, "", "  ")
	e.Timer.StepCustomTiming("graphite", "query", string(b), func() {
		key := req.CacheKey()
		getFn := func() (interface{}, error) {
			return e.Graphite.Query(req)
		}
		var val interface{}
		var hit bool
		val, err, hit = e.Cache.Get(key, getFn)
		expr.CollectCacheHit(e.Cache, "graphite", hit)
		resp = val.(graphite.Response)
	})
	return
}
