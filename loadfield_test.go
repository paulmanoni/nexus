package nexus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/nexus/dataloader"
	"github.com/paulmanoni/nexus/graph"
)

// TestLoadField_BatchesNListChildrenIntoOneCall is the headline
// E2E contract for the DataLoader integration: a query for N
// users that selects each user's bankDetail produces exactly one
// call to the batched fetch function, not N calls. This is the
// N+1 collapse a real GraphQL app needs.
func TestLoadField_BatchesNListChildrenIntoOneCall(t *testing.T) {
	graph.ResetVirtualFieldsForTest()

	const userCount = 5
	var fetchCalls atomic.Int32
	var fetchedKeys atomic.Int32

	mod := Module("loadfield_bench",
		// Parent query — returns N Users. Anonymous-closure
		// returns from constructors lose their name to reflection,
		// so the op name is set explicitly via nexus.Op.
		AsQuery(NewListLfUsers(userCount), Op("listLfUsers")),
		// Virtual field — batched lookup for user.bankDetail.
		LoadField[lfUser, int64, *lfBankDetail](
			"bankDetail",
			func(u lfUser) int64 { return u.ID },
			func(ctx context.Context, userIDs []int64) (map[int64]*lfBankDetail, error) {
				fetchCalls.Add(1)
				fetchedKeys.Add(int32(len(userIDs)))
				out := make(map[int64]*lfBankDetail, len(userIDs))
				for _, id := range userIDs {
					out[id] = &lfBankDetail{
						AccountNo: "ACC-" + itoa64(id),
						Bank:      "Bank-" + itoa64(id),
					}
				}
				return out, nil
			},
		),
	)

	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	// Query: get every user with both a direct field (name) and the
	// virtual batched field (bankDetail). 5 user resolutions should
	// produce exactly 1 fetch.
	body := strings.NewReader(`{"query":"{ listLfUsers { id name bankDetail { accountNo bank } } }"}`)
	resp, err := http.Post(srv.URL+"/graphql", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var env struct {
		Data struct {
			ListLfUsers []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				BankDetail *struct {
					AccountNo string `json:"accountNo"`
					Bank      string `json:"bank"`
				} `json:"bankDetail"`
			} `json:"listLfUsers"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, raw)
	}
	if len(env.Errors) != 0 {
		t.Fatalf("errors: %v", env.Errors)
	}
	if len(env.Data.ListLfUsers) != userCount {
		t.Fatalf("got %d users, want %d (data=%+v)", len(env.Data.ListLfUsers), userCount, env.Data)
	}
	for i, u := range env.Data.ListLfUsers {
		if u.BankDetail == nil {
			t.Fatalf("user %d: bankDetail is nil", i)
		}
		if u.BankDetail.AccountNo != "ACC-"+itoa64(u.ID) {
			t.Errorf("user %d: accountNo mismatch %q", i, u.BankDetail.AccountNo)
		}
	}

	// The headline assertion.
	if got := fetchCalls.Load(); got != 1 {
		t.Errorf("fetch was called %d times, want 1 (N+1 collapse failed)", got)
	}
	if got := fetchedKeys.Load(); got != userCount {
		t.Errorf("fetch saw %d keys total, want %d (one batch of all user IDs)", got, userCount)
	}
}

// TestLoadField_FactoryInjectsFxDeps verifies the constructor-style
// form: a factory function with fx-resolvable params (a fake "DB")
// gets invoked at boot, returns the typed Fetch, and that fetch is
// used to resolve the batched virtual field. Proves the LoadField
// API can play in the same fx graph as AsRest / AsQuery.
func TestLoadField_FactoryInjectsFxDeps(t *testing.T) {
	graph.ResetVirtualFieldsForTest()

	mod := Module("loadfield_fx",
		// Use distinct types so the graphql-go + nexus type
		// registries don't collide with the headline test's
		// lfUser / lfBankDetail caches.
		Provide(newFakeBankDB),
		AsQuery(NewListLfxUsers, Op("listLfxUsers")),
		LoadField[lfxUser, int64, *lfxBankDetail](
			"bankDetail",
			func(u lfxUser) int64 { return u.ID },
			NewLfxBankDetailFetcher, // ← fx injects *fakeBankDB
		),
	)

	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()
	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	body := strings.NewReader(`{"query":"{ listLfxUsers { id bankDetail { accountNo } } }"}`)
	resp, err := http.Post(srv.URL+"/graphql", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env struct {
		Data struct {
			ListLfxUsers []struct {
				ID         int64 `json:"id"`
				BankDetail *struct {
					AccountNo string `json:"accountNo"`
				} `json:"bankDetail"`
			} `json:"listLfxUsers"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, raw)
	}
	if len(env.Errors) != 0 {
		t.Fatalf("errors: %v (body=%s)", env.Errors, raw)
	}
	if len(env.Data.ListLfxUsers) != 3 {
		t.Fatalf("got %d users, want 3 (body=%s)", len(env.Data.ListLfxUsers), raw)
	}
	for i, u := range env.Data.ListLfxUsers {
		if u.BankDetail == nil {
			t.Fatalf("user %d: bankDetail nil", i)
		}
		// fakeBankDB returns "fake-<id>" — proves the fetch reached
		// the injected dep, not some default.
		want := "fake-" + itoa64(u.ID)
		if u.BankDetail.AccountNo != want {
			t.Errorf("user %d: accountNo = %q, want %q", i, u.BankDetail.AccountNo, want)
		}
	}
}

// --- factory-form fixtures ---

type fakeBankDB struct{}

func newFakeBankDB() *fakeBankDB { return &fakeBankDB{} }

func (f *fakeBankDB) BankDetailsByUserIDs(ctx context.Context, ids []int64) (map[int64]*lfxBankDetail, error) {
	out := make(map[int64]*lfxBankDetail, len(ids))
	for _, id := range ids {
		out[id] = &lfxBankDetail{AccountNo: "fake-" + itoa64(id)}
	}
	return out, nil
}

// NewLfxBankDetailFetcher is the fx-factory: declares its dep
// (*fakeBankDB), returns the typed Fetch. nexus.LoadField passes
// this as its factory; fx resolves *fakeBankDB at boot and the
// returned fetch closes over it.
func NewLfxBankDetailFetcher(db *fakeBankDB) dataloader.Fetch[int64, *lfxBankDetail] {
	return func(ctx context.Context, ids []int64) (map[int64]*lfxBankDetail, error) {
		return db.BankDetailsByUserIDs(ctx, ids)
	}
}

func NewListLfxUsers(ctx context.Context, _ struct{}) ([]*lfxUser, error) {
	return []*lfxUser{
		{ID: 1, Name: "a"},
		{ID: 2, Name: "b"},
		{ID: 3, Name: "c"},
	}, nil
}

type lfxUser struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type lfxBankDetail struct {
	AccountNo string `json:"accountNo"`
}

// --- test types ---

// Named at the package level (not anonymous) so reflect.Type.Name()
// returns "lfUser" / "lfBankDetail" — the LoadField parent-name
// derivation depends on that.
type lfUser struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type lfBankDetail struct {
	AccountNo string `json:"accountNo"`
	Bank      string `json:"bank"`
}

func nameFor(i int) string {
	return "user-" + itoa64(int64(i+1))
}

// NewListLfUsers is a constructor whose name nexus derives the query
// name from: "NewListLfUsers" → "listLfUsers". The captured count
// lets the test parametrize how many users to fan out.
func NewListLfUsers(count int) func(ctx context.Context, _ struct{}) ([]*lfUser, error) {
	return func(ctx context.Context, _ struct{}) ([]*lfUser, error) {
		out := make([]*lfUser, count)
		for i := 0; i < count; i++ {
			out[i] = &lfUser{ID: int64(i + 1), Name: nameFor(i)}
		}
		return out, nil
	}
}


func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
