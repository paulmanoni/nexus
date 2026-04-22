package main

import (
	"time"

	"github.com/graphql-go/graphql"
	graph "github.com/paulmanoni/go-graph"
)

// NewGetAllAdverts — real GORM Find against the default DB.
//
// Dependency surface visible from the signature:
//
//	*DBManager    → routes to "main", "questions", etc.
//	*CacheManager → resource "session"
func NewGetAllAdverts(dbs *DBManager, cache *CacheManager) graph.QueryField {
	return graph.NewResolver[AdvertsResponse]("getAllAdverts").
		WithDescription("Get all adverts from the default DB").
		WithNamedMiddleware("auth", "Bearer token validation", AuthMiddleware).
		WithResolver(func(p graph.ResolveParams) (*AdvertsResponse, error) {
			if cached, ok := cache.Get(p.Context, "getAllAdverts"); ok {
				if list, ok := cached.([]Advert); ok {
					return okList(list, "cache hit"), nil
				}
			}
			// dbs.GetDB() promotes through the embedded default DB (= "main")
			// and returns *gorm.DB. No .Using(...) needed.
			var rows []Advert
			if err := dbs.GetDB().Find(&rows).Error; err != nil {
				return nil, err
			}
			_ = cache.Set(p.Context, "getAllAdverts", rows, 5*time.Minute)
			return okList(rows, "fetched from "+dbs.DefaultName()), nil
		}).
		BuildQuery()
}

// NewCreateAdvert — real GORM Create against the default DB.
func NewCreateAdvert(dbs *DBManager, cache *CacheManager) graph.MutationField {
	return graph.NewResolver[AdvertResponse]("createAdvert").
		WithDescription("Create a new advert in the main DB").
		WithNamedMiddleware("auth", "Bearer token validation", AuthMiddleware).
		WithNamedMiddleware("permission:ROLE_CREATE_ADVERT",
			"Requires ROLE_CREATE_ADVERT",
			PermissionMiddleware([]string{"ROLE_CREATE_ADVERT"})).
		WithArgRequired("title", graphql.String).
		WithArgRequired("employerName", graphql.String).
		WithArgValidator("title", graph.Required(), graph.StringLength(3, 120)).
		WithArgValidator("employerName", graph.Required(), graph.StringLength(2, 200)).
		WithResolver(func(p graph.ResolveParams) (*AdvertResponse, error) {
			title, _ := p.Args["title"].(string)
			employer, _ := p.Args["employerName"].(string)
			// Default DB via promoted GetDB(). Explicit: dbs.Using("main").GetDB()
			advert := Advert{Title: title, EmployerName: employer}
			if err := dbs.GetDB().Create(&advert).Error; err != nil {
				return nil, err
			}
			cache.Invalidate("getAllAdverts")
			return okOne(advert, "created"), nil
		}).
		BuildMutation()
}

// NewListQuestions — real GORM Find against the non-default "questions" DB.
func NewListQuestions(dbs *DBManager) graph.QueryField {
	return graph.NewResolver[QuestionsResponse]("listQuestions").
		WithDescription("List questions from the question_bank DB").
		WithNamedMiddleware("auth", "Bearer token validation", AuthMiddleware).
		WithResolver(func(p graph.ResolveParams) (*QuestionsResponse, error) {
			// UsingCtx (not Using) so nexus auto-attaches questions→adverts
			// on first call. See nexus.App.OnResourceUse.
			var qs []Question
			if err := dbs.UsingCtx(p.Context, "questions").GetDB().Find(&qs).Error; err != nil {
				return nil, err
			}
			return okQuestions(qs, "fetched from questions DB"), nil
		}).
		BuildQuery()
}
