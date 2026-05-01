// Package crud carries zero-sized marker types used as the first
// generic parameter of nexus.On[A, T] to identify which CRUD action
// a handler overrides.
//
// Lives in its own subpackage so call sites read naturally:
//
//	nexus.On[crud.Create, User](func(ctx context.Context, db *gorm.DB, u *User) error { ... })
package crud

// Create marks the POST <plural> action that inserts a new record.
type Create struct{}

// Read marks the GET <plural>/:id action that loads one record by id.
type Read struct{}

// Update marks the PATCH <plural>/:id action that merges a partial
// update into an existing record.
type Update struct{}

// Delete marks the DELETE <plural>/:id action that removes a record.
type Delete struct{}

// List marks the GET <plural> action that returns a paginated set.
type List struct{}

// Action is the type-set constraint that admits exactly the five
// CRUD action markers above. Used as the first generic parameter of
// nexus.On so the compiler rejects nexus.On[BogusType, User](...).
type Action interface {
	Create | Read | Update | Delete | List
}