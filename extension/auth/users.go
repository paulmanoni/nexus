package auth

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/paulmanoni/nexus/v2"
)

// Users is the app's account lookup — the one interface the config-driven
// auth path needs (Config.Users). Every scheme ends in Load, so sessions,
// tokens and API keys build identities the same way.
//
//	type Users struct{ db *DB }
//
//	func (u *Users) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) { … }
//	func (u *Users) Load(ctx context.Context, id string) (*auth.Identity, error) { … }
//
//	auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)})
type Users interface {
	// FindLogin returns the account a sign-in names (a username or an
	// email) and its encoded password; (nil, "", nil) when there is none.
	FindLogin(ctx context.Context, login string) (*Identity, string, error)
	// Load rebuilds an identity from its id, for every request a session,
	// token or key authenticates. (nil, nil) when the account is gone.
	Load(ctx context.Context, id string) (*Identity, error)
}

// PasswordSetter is an optional Users method: it stores a newly encoded
// password — auth.SetPassword, and a rehash after a login whose stored hash
// is outdated.
type PasswordSetter interface {
	SetPassword(ctx context.Context, id, encoded string) error
}

// LoginChecker is an optional Users method auth.Login calls after the
// password matches: return an error (nexus.Err(nexus.Forbidden, "this
// account is disabled")) to refuse the sign-in.
type LoginChecker interface {
	CheckLogin(ctx context.Context, id *Identity) error
}

// UsersOption names the app's Users for Config.Users.
type UsersOption struct {
	value Users
	ctor  any
	set   bool
}

// UseUsers takes a DI constructor whose result implements Users; nexus
// provides it, so other code can take the concrete type too. A result
// that doesn't implement Users fails boot, naming the missing method.
func UseUsers(constructor any) UsersOption { return UsersOption{ctor: constructor, set: true} }

// StaticUsers takes a ready Users value — tests, or one built without DI.
func StaticUsers(u Users) UsersOption { return UsersOption{value: u, set: true} }

var usersType = reflect.TypeFor[Users]()

// usersOption wires Config.Users into state: inline for a value, through a
// DI invoke for a constructor, either way before the first request.
func usersOption(state *moduleState, o UsersOption) (nexus.Option, error) {
	if o.value != nil {
		if err := checkOptionalMethods(reflect.TypeOf(o.value)); err != nil {
			return nil, err
		}
		state.config.users = o.value
		return nexus.Options(), nil
	}
	ct := reflect.TypeOf(o.ctor)
	if ct == nil || ct.Kind() != reflect.Func || ct.NumOut() == 0 {
		return nil, fmt.Errorf("UseUsers: want a constructor function returning your Users, got %T", o.ctor)
	}
	ut := ct.Out(0)
	if !ut.Implements(usersType) {
		return nil, fmt.Errorf("UseUsers: %s does not implement auth.Users: %s", ut, missingMethods(ut, usersType))
	}
	if err := checkOptionalMethods(ut); err != nil {
		return nil, err
	}
	invoke := reflect.MakeFunc(reflect.FuncOf([]reflect.Type{ut}, nil, false), func(args []reflect.Value) []reflect.Value {
		state.config.users = args[0].Interface().(Users)
		return nil
	})
	return nexus.Options(nexus.Provide(o.ctor), nexus.Invoke(invoke.Interface())), nil
}

// optionalMethods are the Users methods nexus uses when present.
var optionalMethods = []reflect.Type{
	reflect.TypeFor[PasswordSetter](),
	reflect.TypeFor[LoginChecker](),
	reflect.TypeFor[PublicUser](),
	reflect.TypeFor[Provisioner](),
}

// checkOptionalMethods fails boot for an optional method spelled right but
// with another signature — nexus would otherwise skip it without a word.
func checkOptionalMethods(t reflect.Type) error {
	for _, iface := range optionalMethods {
		m := iface.Method(0)
		if _, has := t.MethodByName(m.Name); has && !t.Implements(iface) {
			return fmt.Errorf("%s has a %s method nexus can't use: %s", t, m.Name, missingMethods(t, iface))
		}
	}
	return nil
}

// missingMethods names the methods of iface that t lacks or has with
// another signature.
func missingMethods(t, iface reflect.Type) string {
	var out []string
	for i := range iface.NumMethod() {
		want := iface.Method(i)
		got, ok := t.MethodByName(want.Name)
		switch {
		case !ok:
			out = append(out, "no method "+want.Name+strings.TrimPrefix(want.Type.String(), "func"))
		case !methodMatches(got.Type, want.Type, t.Kind() != reflect.Interface):
			out = append(out, want.Name+" has signature "+got.Type.String()+", want "+want.Type.String())
		}
	}
	return strings.Join(out, "; ")
}

// methodMatches compares a method's type with an interface method's,
// skipping the receiver a concrete type's method carries.
func methodMatches(got, want reflect.Type, hasReceiver bool) bool {
	skip := 0
	if hasReceiver {
		skip = 1
	}
	if got.NumIn()-skip != want.NumIn() || got.NumOut() != want.NumOut() {
		return false
	}
	for i := range want.NumIn() {
		if got.In(i+skip) != want.In(i) {
			return false
		}
	}
	for i := range want.NumOut() {
		if got.Out(i) != want.Out(i) {
			return false
		}
	}
	return true
}
