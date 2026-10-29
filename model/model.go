// Package model is the catalog of Muse models agnts-muse can run and what
// each one costs.
//
// Muse's own catalog (MSP model/list) names the models and their limits,
// but reports cost: null on subscription accounts, so prices live here, in
// a versioned table that configuration can override without a release.
package model

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// ID is a Muse model id as the provider names it, e.g. "muse-spark-1.3".
type ID string

// Models known at release.
const (
	Spark      ID = "muse-spark-1.3"
	Spark12    ID = "muse-spark-1.2"
	Wonder     ID = "muse-wonder-1.0"
	WonderFull ID = "muse-wonder-full-1.0"
	// SparkOpen is Muse Spark Open, Meta's open-weights Spark, as Baseten
	// serves it for the Meta hackathon.
	SparkOpen ID = "meta-models/muse-spark-open-1026"
)

// Tier orders models by capability and price; routing escalates upward.
type Tier int

// Tiers.
const (
	TierSpark Tier = iota + 1
	TierWonder
)

func (t Tier) String() string {
	switch t {
	case TierSpark:
		return "spark"
	case TierWonder:
		return "wonder"
	}
	return fmt.Sprintf("tier(%d)", int(t))
}

// Usage is the token counts of one or more model calls, as MSP's
// session/tokenUsage reports them. Input includes Cached: the uncached
// part is Input - Cached.
type Usage struct {
	Input     int64 `json:"input"`
	Cached    int64 `json:"cached"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
}

// Add returns the sum of u and v.
func (u Usage) Add(v Usage) Usage {
	return Usage{u.Input + v.Input, u.Cached + v.Cached, u.Output + v.Output, u.Reasoning + v.Reasoning}
}

// Price is a model's per-million-token prices.
type Price struct {
	Input  money.Rate `json:"input"`
	Cached money.Rate `json:"cached"`
	Output money.Rate `json:"output"`
	// Reasoning bills reasoning tokens on top of Output. Muse reports them
	// inside the output count today, so it is zero; a provider that bills
	// them separately sets it.
	Reasoning money.Rate `json:"reasoning,omitempty"`
}

// Cost is the exact price of u.
func (p Price) Cost(u Usage) money.Exact {
	uncached := max(u.Input-u.Cached, 0)
	return p.Input.Cost(uncached) + p.Cached.Cost(u.Cached) + p.Output.Cost(u.Output) + p.Reasoning.Cost(u.Reasoning)
}

// Info describes one model.
type Info struct {
	ID           ID
	Alias        string // short name: "spark", "wonder"
	Tier         Tier
	Price        Price
	ContextLimit int64
}

// ErrUnknownModel reports a model the catalog has no entry for.
var ErrUnknownModel = errors.New("model: unknown model")

// Catalog is an immutable set of models. Build one with [NewCatalog] or use
// [Default]; derive a changed copy with [Catalog.WithPrices].
type Catalog struct {
	byID    map[ID]Info
	byAlias map[string]ID
	dear    Price // the most expensive price, for unknown models
}

// NewCatalog builds a catalog. The first model of each alias wins.
func NewCatalog(models ...Info) *Catalog {
	c := &Catalog{byID: map[ID]Info{}, byAlias: map[string]ID{}}
	for _, m := range models {
		c.byID[m.ID] = m
		if _, ok := c.byAlias[m.Alias]; m.Alias != "" && !ok {
			c.byAlias[m.Alias] = m.ID
		}
		c.dear = dearer(c.dear, m.Price)
	}
	return c
}

func dearer(a, b Price) Price {
	return Price{max(a.Input, b.Input), max(a.Cached, b.Cached), max(a.Output, b.Output), max(a.Reasoning, b.Reasoning)}
}

// Default is the catalog at release (Muse Code 1.4.13, Oct 2026), with
// Meta Model API list prices, and Muse Spark Open at Baseten's.
var Default = NewCatalog(
	Info{ID: Spark, Alias: "spark", Tier: TierSpark, ContextLimit: 1_007_997,
		Price: Price{Input: money.PerMillion(1.25), Cached: money.PerMillion(0.15), Output: money.PerMillion(4.25)}},
	Info{ID: Spark12, Alias: "spark-1.2", Tier: TierSpark, ContextLimit: 1_007_997,
		Price: Price{Input: money.PerMillion(1.25), Cached: money.PerMillion(0.15), Output: money.PerMillion(4.25)}},
	Info{ID: Wonder, Alias: "wonder", Tier: TierWonder, ContextLimit: 1_007_997,
		Price: Price{Input: money.PerMillion(4), Cached: money.PerMillion(0.20), Output: money.PerMillion(20)}},
	Info{ID: WonderFull, Alias: "wonder-full", Tier: TierWonder, ContextLimit: 1_007_997,
		Price: Price{Input: money.PerMillion(4), Cached: money.PerMillion(0.20), Output: money.PerMillion(20)}},
	// Baseten's list price (Oct 2026), the same as Spark's on Meta.
	Info{ID: SparkOpen, Alias: "spark-open", Tier: TierSpark, ContextLimit: 1_007_997,
		Price: Price{Input: money.PerMillion(1.25), Cached: money.PerMillion(0.15), Output: money.PerMillion(4.25)}},
)

// Lookup returns the model with id.
func (c *Catalog) Lookup(id ID) (Info, bool) {
	m, ok := c.byID[id]
	return m, ok
}

// Resolve accepts an id or an alias ("spark") and returns the model.
func (c *Catalog) Resolve(name string) (Info, error) {
	if m, ok := c.byID[ID(name)]; ok {
		return m, nil
	}
	if id, ok := c.byAlias[strings.ToLower(name)]; ok {
		return c.byID[id], nil
	}
	return Info{}, fmt.Errorf("%w: %q", ErrUnknownModel, name)
}

// Models returns every model, cheapest tier first, then by id.
func (c *Catalog) Models() []Info {
	out := slices.Collect(maps.Values(c.byID))
	slices.SortFunc(out, func(a, b Info) int {
		if a.Tier != b.Tier {
			return int(a.Tier - b.Tier)
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return out
}

// Cost prices u on model id. A model the catalog does not know (a new
// Muse release) is priced at the catalog's highest rates and reported as
// estimated: a budget may over-reserve, never under-reserve.
func (c *Catalog) Cost(id ID, u Usage) (cost money.Exact, estimated bool) {
	if m, ok := c.byID[id]; ok {
		return m.Price.Cost(u), false
	}
	return c.dear.Cost(u), true
}

// WithPrices returns a copy of c with prices overridden from spec, a list
// such as "muse-spark-1.3=1.25/0.15/4.25; muse-wonder-1.0=4/0.2/20" (input,
// cached, output in dollars per million tokens). An id the catalog lacks is
// added at TierWonder, so routing never treats it as cheap.
func (c *Catalog) WithPrices(spec string) (*Catalog, error) {
	models := maps.Clone(c.byID)
	for entry := range strings.SplitSeq(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rates, ok := strings.Cut(entry, "=")
		parts := strings.Split(rates, "/")
		if !ok || len(parts) != 3 {
			return nil, fmt.Errorf("model: price %q: want id=input/cached/output", entry)
		}
		var r [3]money.Rate
		for i, p := range parts {
			v, err := money.ParseRate(p)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("model: price %q: %q is not a price", entry, p)
			}
			r[i] = v
		}
		id := ID(strings.TrimSpace(name))
		m, known := models[id]
		if !known {
			m = Info{ID: id, Tier: TierWonder}
		}
		m.Price = Price{Input: r[0], Cached: r[1], Output: r[2], Reasoning: m.Price.Reasoning}
		models[id] = m
	}
	return NewCatalog(slices.Collect(maps.Values(models))...), nil
}
