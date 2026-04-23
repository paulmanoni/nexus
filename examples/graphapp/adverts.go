package main

import (
	"time"

	"github.com/paulmanoni/nexus"
)

// AdvertsService is the nexus service wrapper for the adverts domain. Every
// resolver below takes it as its first parameter so the auto-mount routes
// the field into *AdvertsService's value group.
//
// Resolvers are plain Go functions — no builder chain, no go-graph imports
// at call sites. nexus.AsQuery / AsMutation reflect on the signature and
// wire the typed resolver under the hood.

// NewGetAllAdverts — typed DB + cache, no args.
//
// The signature names every dep explicitly:
//
//	*AdvertsService        → grounds this op on the adverts service node
//	*MainDB                → typed primary DB; edge main ← adverts auto-drawn
//	*CacheManager          → session cache; edge session ← adverts auto-drawn
//	nexus.Params[struct{}] → resolve context + schema info, no user args
func NewGetAllAdverts(svc *AdvertsService, main *MainDB, cache *CacheManager, p nexus.Params[struct{}]) (*AdvertsResponse, error) {
	if cached, ok := cache.Get(p.Context, "getAllAdverts"); ok {
		if list, ok := cached.([]Advert); ok {
			return okList(list, "cache hit"), nil
		}
	}
	var rows []Advert
	if err := main.GetDB().WithContext(p.Context).Find(&rows).Error; err != nil {
		return nil, err
	}
	_ = cache.Set(p.Context, "getAllAdverts", rows, 5*time.Minute)
	return okList(rows, "fetched from main"), nil
}

// CreateAdvertArgs is the reflective replacement for WithArgRequired +
// WithArgValidator chains. Each field's tags drive schema + validators.
type CreateAdvertArgs struct {
	Title        string `graphql:"title,required"        validate:"required,len=3|120"`
	EmployerName string `graphql:"employerName,required" validate:"required,len=2|200"`
}

// NewCreateAdvert — create an advert with tag-declared validators.
//
// Uses the single-input-object shape: the anonymous wrapper
// `struct { Input CreateAdvertArgs }` tells nexus to expose one GraphQL
// arg named `input` of type CreateAdvertArgsInput. Clients then call:
//
//	mutation { createAdvert(input: { title: "...", employerName: "..." }) { ... } }
func NewCreateAdvert(svc *AdvertsService, main *MainDB, cache *CacheManager, p nexus.Params[CreateAdvertArgs]) (*AdvertResponse, error) {
	advert := Advert{Title: p.Args.Title, EmployerName: p.Args.EmployerName}
	if err := main.GetDB().WithContext(p.Context).Create(&advert).Error; err != nil {
		return nil, err
	}
	cache.Invalidate("getAllAdverts")
	return okOne(advert, "created"), nil
}

// NewListQuestions — hits the non-default "questions" DB, named by type.
// No UsingCtx, no string lookup; the *QuestionsDB dep is what attaches
// the questions → adverts edge on the dashboard.
func NewListQuestions(questions *QuestionsDB) (*QuestionsResponse, error) {
	var qs []Question
	if err := questions.GetDB().Find(&qs).Error; err != nil {
		return nil, err
	}
	return okQuestions(qs, "fetched from questions DB"), nil
}
