package events_test

import (
	"slices"
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

const (
	motionTopic = "tns1:RuleEngine/CellMotionDetector/Motion"
	smartTopic  = "tns1:RuleEngine/TPSmartEventDetector/TPSmartEvent"
)

func item(name, value string) events.Item { return events.Item{Name: name, Value: value} }

func TestTapoRules(t *testing.T) {
	rules := events.TapoRules()
	tests := []struct {
		name  string
		topic string
		items []events.Item
		want  []events.MatchResult
	}{
		{"motion active", motionTopic, []events.Item{item("IsMotion", "true")}, []events.MatchResult{{Kind: events.KindMotion, Active: true}}},
		{"motion inactive", motionTopic, []events.Item{item("IsMotion", "false")}, []events.MatchResult{{Kind: events.KindMotion, Active: false}}},
		{"person", smartTopic, []events.Item{item("IsPeople", "true")}, []events.MatchResult{{Kind: events.KindPerson, Active: true}}},
		{"pet", smartTopic, []events.Item{item("IsPet", "true")}, []events.MatchResult{{Kind: events.KindPet, Active: true}}},
		{"vehicle", smartTopic, []events.Item{item("IsVehicle", "true")}, []events.MatchResult{{Kind: events.KindVehicle, Active: true}}},
		{"catch-all other", smartTopic, []events.Item{item("IsBabyCry", "true")}, []events.MatchResult{{Kind: events.KindOther, Active: true}}},
		{"catch-all other inactive", smartTopic, []events.Item{item("IsBabyCry", "false")}, []events.MatchResult{{Kind: events.KindOther, Active: false}}},
		{"topic with surrounding space", " " + motionTopic + "\n", []events.Item{item("IsMotion", "true")}, []events.MatchResult{{Kind: events.KindMotion, Active: true}}},
		{"unrelated topic", "tns1:VideoSource/ImageTooDark/ImagingService", []events.Item{item("State", "true")}, nil},
		{"motion topic, unknown item", motionTopic, []events.Item{item("Other", "true")}, nil},
		{"suffix must be whole segment", "tns1:RuleEngine/XCellMotionDetector/Motion", []events.Item{item("IsMotion", "true")}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := events.Match(rules, tc.topic, tc.items)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Match(%q, %v) = %v, want %v", tc.topic, tc.items, got, tc.want)
			}
		})
	}
}

func TestTapoRulesCatchAllIsLast(t *testing.T) {
	rules := events.TapoRules()
	last := rules[len(rules)-1]
	if !last.AnyItem || last.Item != "" || last.ActiveValue != "true" || last.Kind != events.KindOther {
		t.Fatalf("last Tapo rule = %+v, want the catch-all (AnyItem, ActiveValue true, other)", last)
	}
	for _, r := range rules[:len(rules)-1] {
		if r.AnyItem {
			t.Fatalf("catch-all rule %+v is not last", r)
		}
	}
}

func TestMatchFirstRuleWins(t *testing.T) {
	rules, err := events.CompileRules([]config.RuleConfig{
		{Kind: "person", Topic: "Smart", Item: "IsPeople", ActiveValue: "true"},
		{Kind: "other", Topic: "Smart", ActiveValue: "true"},
		{Kind: "motion", Topic: "Smart", Item: "IsPeople", ActiveValue: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := events.Match(rules, "tns1:Smart", []events.Item{item("IsPeople", "true"), item("IsDog", "1")})
	want := []events.MatchResult{{Kind: events.KindPerson, Active: true}, {Kind: events.KindOther, Active: false}}
	if !slices.Equal(got, want) {
		t.Fatalf("Match = %v, want %v", got, want)
	}
}

func TestCompileRules(t *testing.T) {
	rules, err := events.CompileRules([]config.RuleConfig{
		{Kind: "motion", Topic: "CellMotionDetector/Motion$", Item: "IsMotion", ActiveValue: "1"},
		{Kind: "other", Topic: "Tamper", ActiveValue: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	if r := rules[0]; r.Kind != events.KindMotion || r.Item != "IsMotion" || r.AnyItem || r.ActiveValue != "1" || !r.Topic.MatchString(motionTopic) {
		t.Fatalf("rule 0 = %+v", r)
	}
	if r := rules[1]; r.Kind != events.KindOther || !r.AnyItem || r.ActiveValue != "true" {
		t.Fatalf("rule 1 = %+v, want AnyItem with the default active value", r)
	}
	if got := events.Match(rules, motionTopic, []events.Item{item("IsMotion", "1")}); !slices.Equal(got, []events.MatchResult{{Kind: events.KindMotion, Active: true}}) {
		t.Fatalf("Match = %v", got)
	}
}

func TestCompileRulesRejects(t *testing.T) {
	for name, rc := range map[string]config.RuleConfig{
		"bad regex":    {Kind: "motion", Topic: "(", Item: "IsMotion"},
		"unknown kind": {Kind: "dog", Topic: "Motion", Item: "IsMotion"},
	} {
		if _, err := events.CompileRules([]config.RuleConfig{rc}); err == nil {
			t.Errorf("%s: CompileRules(%+v) succeeded, want an error", name, rc)
		}
	}
}
