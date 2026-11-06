package events

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/apikdech/zrelay-v2/internal/relay/config"
)

// Rule maps a notification data item to a trigger kind.
type Rule struct {
	Kind Kind
	// Topic is matched against the notification topic, trimmed of space.
	Topic *regexp.Regexp
	// Item is the SimpleItem name the rule reads. It is "" with AnyItem set
	// for a rule that reads every item.
	Item    string
	AnyItem bool
	// ActiveValue is the item value meaning "active"; any other value is
	// inactive.
	ActiveValue string
}

// Item is one SimpleItem of a notification's Data.
type Item struct{ Name, Value string }

// MatchResult is the state one data item gives its kind.
type MatchResult struct {
	Kind   Kind
	Active bool
}

// CompileRules compiles configured rules. A rule without an item reads
// every item; an empty active value means config.DefaultRuleActiveValue.
func CompileRules(rcs []config.RuleConfig) ([]Rule, error) {
	rules := make([]Rule, 0, len(rcs))
	for i, rc := range rcs {
		k := Kind(rc.Kind)
		if !slices.Contains(allKinds, k) {
			return nil, fmt.Errorf("events: rule %d: unknown kind %q", i, rc.Kind)
		}
		re, err := regexp.Compile(rc.Topic)
		if err != nil {
			return nil, fmt.Errorf("events: rule %d: topic: %w", i, err)
		}
		active := rc.ActiveValue
		if active == "" {
			active = config.DefaultRuleActiveValue
		}
		rules = append(rules, Rule{Kind: k, Topic: re, Item: rc.Item, AnyItem: rc.Item == "", ActiveValue: active})
	}
	return rules, nil
}

// Tapo topic patterns: the topic ends with the suffix as whole segments,
// after any namespace prefix such as "tns1:".
var (
	tapoMotion = regexp.MustCompile(`(^|[:/])RuleEngine/CellMotionDetector/Motion$`)
	tapoSmart  = regexp.MustCompile(`(^|[:/])RuleEngine/TPSmartEventDetector/TPSmartEvent$`)
)

// TapoRules returns the built-in Tapo rules (design §4.3). The catch-all
// row for any other smart-event item is last, so the named items win.
func TapoRules() []Rule {
	return []Rule{
		{Kind: KindMotion, Topic: tapoMotion, Item: "IsMotion", ActiveValue: "true"},
		{Kind: KindPerson, Topic: tapoSmart, Item: "IsPeople", ActiveValue: "true"},
		{Kind: KindPet, Topic: tapoSmart, Item: "IsPet", ActiveValue: "true"},
		{Kind: KindVehicle, Topic: tapoSmart, Item: "IsVehicle", ActiveValue: "true"},
		{Kind: KindOther, Topic: tapoSmart, AnyItem: true, ActiveValue: "true"},
	}
}

// Match classifies a notification's data items. For each item the first
// rule whose topic and item match wins; items no rule matches are skipped.
func Match(rules []Rule, topic string, items []Item) []MatchResult {
	topic = strings.TrimSpace(topic)
	var out []MatchResult
	for _, it := range items {
		for _, r := range rules {
			if (r.AnyItem || r.Item == it.Name) && r.Topic.MatchString(topic) {
				out = append(out, MatchResult{Kind: r.Kind, Active: it.Value == r.ActiveValue})
				break
			}
		}
	}
	return out
}
