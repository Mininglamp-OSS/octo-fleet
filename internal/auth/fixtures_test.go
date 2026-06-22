package auth

// Test-only fixture types. The mockSrv helper in middleware_test.go
// constructs JSON response bodies from these shapes; the JSON tags
// match the octo-auth SDK's contract types 1:1 so the SDK decoder
// accepts them. The production middleware reads through SDK contract
// types and does not reference these.
//
// Prior to PR-D2 these types lived in middleware.go and were
// production-unreachable but indistinguishable from real types when
// reading the file. Moving to a _test.go file makes the test-only
// status visible at a glance and excludes the types from production
// builds.

type ownedBot struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type verifyTokenResp struct {
	UID              string              `json:"uid"`
	Name             string              `json:"name"`
	Role             string              `json:"role"`
	OwnedBots        []ownedBot          `json:"owned_bots"`
	ContextIncluded  bool                `json:"context_included"`
	Spaces           []string            `json:"spaces,omitempty"`
	OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space,omitempty"`
}

type verifyBotResp struct {
	BotUID    string `json:"bot_uid"`
	BotName   string `json:"bot_name"`
	OwnerUID  string `json:"owner_uid"`
	OwnerName string `json:"owner_name"`
	SpaceID   string `json:"space_id"`
}

type verifyAPIKeyResp struct {
	UID              string              `json:"uid"`
	SpaceID          string              `json:"space_id"`
	OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space,omitempty"`
	ContextIncluded  bool                `json:"context_included"`
}
