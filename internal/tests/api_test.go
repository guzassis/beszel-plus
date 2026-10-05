package tests

import (
	"net/http"
	"testing"

	pbtests "github.com/pocketbase/pocketbase/tests"
)

func TestAPIScenariosKeepRouterHooksLocalWhenReusingApp(t *testing.T) {
	app, err := pbtests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	before := app.OnServe().Length()
	for _, name := range []string{"first request", "second request"} {
		scenario := ApiScenario{
			Name: name, Method: http.MethodGet, URL: "/api/health",
			ExpectedStatus: http.StatusOK, ExpectedContent: []string{`"code":200`},
			TestAppFactory: func(testing.TB) *pbtests.TestApp { return app },
		}
		scenario.Test(t)
	}
	if after := app.OnServe().Length(); after != before {
		t.Fatalf("router hooks accumulated: before=%d after=%d", before, after)
	}
}
