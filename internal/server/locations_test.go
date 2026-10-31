package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"svngo/internal/svn"
)

func TestLocations(t *testing.T) {
	requireSVN(t)
	cfg := testConfig(t, t.TempDir())
	e := startEnv(t, cfg)
	admin := e.client()
	admin.login("admin", "Admin-Pass-1")
	repoDir := func(parts ...string) string { return filepath.Join(append([]string{cfg.ReposDir}, parts...)...) }
	locationOf := func(name string) string {
		t.Helper()
		var loc string
		if err := e.app.db.QueryRow("SELECT location FROM repos WHERE name=?", name).Scan(&loc); err != nil {
			t.Fatalf("repo %s: %v", name, err)
		}
		return loc
	}

	admin.postOK("/admin/repos", url.Values{"name": {"core"}})

	// invalid, reserved and repository-folder names are rejected
	for name, want := range map[string]string{
		"admin": "예약된 이름", "SVN": "예약된 이름", "a b": "위치 이름은", "../x": "위치 이름은", "core": "저장소 폴더",
	} {
		if msg := admin.postErr("/admin/system/locations", url.Values{"name": {name}}); !strings.Contains(msg, want) {
			t.Errorf("location %q: %q", name, msg)
		}
	}
	os.WriteFile(repoDir("afile"), []byte("x"), 0o644)
	if msg := admin.postErr("/admin/system/locations", url.Values{"name": {"afile"}}); !strings.Contains(msg, "폴더가 아닙니다") {
		t.Errorf("file as location: %q", msg)
	}

	// create a location; the folder is made and Apache gets a <Location>
	page := admin.postOK("/admin/system/locations", url.Values{"name": {"android"}})
	if !strings.Contains(extractAlert(page), "위치 /android 을(를) 추가했습니다 (저장소 0개 등록)") {
		t.Fatalf("create flash: %q", extractAlert(page))
	}
	if st, err := os.Stat(repoDir("android")); err != nil || !st.IsDir() {
		t.Fatal("location folder not created")
	}
	if msg := admin.postErr("/admin/system/locations", url.Values{"name": {"android"}}); !strings.Contains(msg, "이미 등록된 위치") {
		t.Fatalf("duplicate location: %q", msg)
	}
	if _, page := admin.get("/admin/system"); !strings.Contains(page, `action="/admin/system/locations/android/delete"`) || !strings.Contains(page, "/android/&lt;") {
		t.Fatal("location not listed on the system page")
	}

	// repositories in a location; names stay unique across locations
	admin.postOK("/admin/repos", url.Values{"name": {"app"}, "location": {"android"}, "layout": {"1"}})
	if !svn.IsRepoDir(repoDir("android", "app")) || locationOf("app") != "android" {
		t.Fatal("repository not created in the location")
	}
	if _, err := os.Stat(repoDir("android", ".svngo-tmp")); err != nil {
		t.Fatal("location work folder missing")
	}
	if _, page := admin.get("/admin/repos"); !strings.Contains(page, e.srv.URL+"/android/app") || !strings.Contains(page, e.srv.URL+"/svn/core") || !strings.Contains(page, `<option value="android">/android/</option>`) {
		t.Fatal("repository URLs or location select missing")
	}
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"name": {"app"}}, "이미 등록된"},
		{url.Values{"name": {"core"}, "location": {"android"}}, "이미 등록된"},
		{url.Values{"name": {"android"}}, "위치 이름이라"},
		{url.Values{"name": {"x1"}, "location": {"nope"}}, "등록되지 않은 위치"},
		{url.Values{"name": {"x1"}, "location": {"../etc"}}, "등록되지 않은 위치"},
	} {
		if msg := admin.postErr("/admin/repos", c.form); !strings.Contains(msg, c.want) {
			t.Errorf("create %v: %q", c.form, msg)
		}
	}

	// commit through file:// and read it back through the web UI
	wc := filepath.Join(t.TempDir(), "wc")
	run(t, "", "svn", "checkout", "file://"+repoDir("android", "app"), wc)
	os.WriteFile(filepath.Join(wc, "trunk", "main.c"), []byte("int main(){}\n"), 0o644)
	run(t, wc, "svn", "add", "trunk/main.c")
	run(t, wc, "svn", "commit", "-m", "location commit", "--username", "admin")
	if _, page := admin.get("/admin/repos/app/commits"); !strings.Contains(page, "location commit") {
		t.Fatal("commit in a location repository not shown")
	}
	if _, page := admin.get("/admin/repos/app/files/trunk"); !strings.Contains(page, "main.c") || !strings.Contains(page, e.srv.URL+"/android/app/trunk") {
		t.Fatal("browse of a location repository")
	}
	if _, page := admin.get("/admin/repos/app"); !strings.Contains(page, "svn checkout "+e.srv.URL+"/android/app/trunk") {
		t.Fatal("overview checkout URL")
	}

	// configured base URL: other locations replace its trailing /svn
	admin.postOK("/admin/system/settings", withValues(settingsForm, "svn_base_url", "https://svn.example.com/svn"))
	if _, page := admin.get("/admin/repos"); !strings.Contains(page, "https://svn.example.com/android/app") || !strings.Contains(page, "https://svn.example.com/svn/core") {
		t.Fatal("location URL from svn_base_url")
	}
	admin.postOK("/admin/system/settings", withValues(settingsForm, "svn_base_url", ""))

	// move between locations through the rename form
	page = admin.postOK("/admin/repos/app/rename", url.Values{"new_name": {"app"}, "location": {""}})
	if !strings.Contains(extractAlert(page), "/svn/app 로 이동") || !svn.IsRepoDir(repoDir("app")) || locationOf("app") != "" {
		t.Fatalf("move to /svn: %q", extractAlert(page))
	}
	admin.postOK("/admin/repos/app/rename", url.Values{"new_name": {"app2"}, "location": {"android"}})
	if !svn.IsRepoDir(repoDir("android", "app2")) || locationOf("app2") != "android" {
		t.Fatal("move and rename into the location")
	}
	if msg := admin.postErr("/admin/repos/app2/rename", url.Values{"new_name": {"app2"}, "location": {"android"}}); !strings.Contains(msg, "기존 이름과 같습니다") {
		t.Fatalf("same name and location: %q", msg)
	}
	if msg := admin.postErr("/admin/repos/app2/rename", url.Values{"new_name": {"app2"}, "location": {"nope"}}); !strings.Contains(msg, "등록되지 않은 위치") {
		t.Fatalf("move to an unknown location: %q", msg)
	}
	admin.postOK("/admin/repos/app2/rename", url.Values{"new_name": {"app"}}) // no location field: stays
	if !svn.IsRepoDir(repoDir("android", "app")) || locationOf("app") != "android" {
		t.Fatal("rename without a location field moved the repository")
	}
	if _, page := admin.get("/admin/repos/app/settings"); !strings.Contains(page, `<option value="android" selected>`) {
		t.Fatal("current location not selected in the move form")
	}

	// backup, restore into a location, overwrite keeps the location, verify
	admin.postOK("/admin/repos/app/backup", nil)
	if jobs := waitJobs(t, e); jobs[0].Status != "ok" {
		t.Fatalf("backup: %+v", jobs[0])
	}
	backups, _ := e.app.listBackups("app")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if msg := admin.postErr("/admin/system/restore", url.Values{"file": {backups[0].Name}, "target": {"app-copy"}, "location": {"nope"}}); !strings.Contains(msg, "등록되지 않은 위치") {
		t.Fatalf("restore into an unknown location: %q", msg)
	}
	admin.postOK("/admin/system/restore", url.Values{"file": {backups[0].Name}, "target": {"app-copy"}, "location": {"android"}})
	if jobs := waitJobs(t, e); jobs[0].Status != "ok" {
		t.Fatalf("restore: %+v", jobs[0])
	}
	if svn.FSFSYoungest(repoDir("android", "app-copy")) != 2 || locationOf("app-copy") != "android" {
		t.Fatal("restore into the location")
	}
	admin.postOK("/admin/system/restore", url.Values{"file": {backups[0].Name}, "target": {"app-copy"}, "overwrite": {"1"}, "confirm": {"app-copy"}})
	if jobs := waitJobs(t, e); jobs[0].Status != "ok" {
		t.Fatalf("overwrite restore: %+v", jobs[0])
	}
	if !svn.IsRepoDir(repoDir("android", "app-copy")) || svn.IsRepoDir(repoDir("app-copy")) || locationOf("app-copy") != "android" {
		t.Fatal("overwrite restore changed the location")
	}
	admin.postOK("/admin/repos/app-copy/verify", nil)
	if jobs := waitJobs(t, e); jobs[0].Status != "ok" {
		t.Fatalf("verify: %+v", jobs[0])
	}

	// a location that still has repositories cannot be removed
	if msg := admin.postErr("/admin/system/locations/android/delete", nil); !strings.Contains(msg, "저장소가 2개 남아") {
		t.Fatalf("delete non-empty location: %q", msg)
	}

	// folders copied from another server: candidates, registration, conflicts
	os.MkdirAll(repoDir("hw"), 0o755)
	run(t, "", "svnadmin", "create", repoDir("hw", "fw"))
	run(t, "", "svnadmin", "create", repoDir("hw", "core"))
	if _, page := admin.get("/admin/system"); !strings.Contains(page, "/hw 등록") {
		t.Fatal("location candidate not offered")
	}
	page = admin.postOK("/admin/system/locations", url.Values{"name": {"hw"}})
	if !strings.Contains(extractAlert(page), "저장소 1개 등록") || locationOf("fw") != "hw" || locationOf("core") != "" {
		t.Fatalf("register candidate: %q", extractAlert(page))
	}
	if _, page := admin.get("/admin/system"); !strings.Contains(page, "hw/core") || strings.Contains(page, "/hw 등록") {
		t.Fatal("name conflict not reported")
	}
	if st, _, page := admin.post("/admin/system/sync", nil); st != http.StatusOK || !strings.Contains(page, "이름 충돌") || !strings.Contains(page, "hw/core") {
		t.Fatal("sync check does not report the conflict")
	}
	os.RemoveAll(repoDir("hw", "core"))

	// emptied locations can be removed; the folder goes only when empty
	admin.postOK("/admin/repos/app/delete", url.Values{"confirm": {"app"}})
	admin.postOK("/admin/repos/app-copy/delete", url.Values{"confirm": {"app-copy"}})
	page = admin.postOK("/admin/system/locations/android/delete", nil)
	if !strings.Contains(extractAlert(page), "위치 /android 을(를) 삭제했습니다") {
		t.Fatalf("delete flash: %q", extractAlert(page))
	}
	if _, err := os.Stat(repoDir("android")); !os.IsNotExist(err) {
		t.Fatal("empty location folder not removed")
	}
	if msg := admin.postErr("/admin/system/locations/android/delete", nil); !strings.Contains(msg, "등록되지 않은 위치") {
		t.Fatalf("second delete: %q", msg)
	}
	if _, page := admin.get("/admin/system/logs"); !strings.Contains(page, "위치 추가") || !strings.Contains(page, "위치 삭제") {
		t.Fatal("location changes not audited")
	}
	if st, _, page := admin.post("/admin/system/sync", nil); st != http.StatusOK || !strings.Contains(page, "모든 검증을 통과했습니다") {
		t.Fatalf("sync check: %s", extractAlert(page))
	}

	// restart: locations and placements survive
	e.stop()
	e2 := startEnv(t, cfg)
	names, err := locationNames(context.Background(), e2.app.db)
	if err != nil || len(names) != 1 || names[0] != "hw" {
		t.Fatalf("locations after restart: %v %v", names, err)
	}
	admin2 := e2.client()
	admin2.login("admin", "Admin-Pass-1")
	if _, page := admin2.get("/admin/repos/fw"); !strings.Contains(page, e2.srv.URL+"/hw/fw") {
		t.Fatal("location repository URL after restart")
	}
}
