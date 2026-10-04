package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/scanner"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A scan the service lost, through the service as main wires it with the
// bearer tokens of a realm: at startup the scan a previous process of this
// host left running is failed, interrupted, and the reaper fails the one
// another host's process stopped speaking for, timed out; an admin reads
// both as failed and why, a scan that walks still running; a viewer reads
// nothing. The scan Job still starts a scan with the service account.
func TestALostScanSaysWhyThroughTheService(t *testing.T) {
	in := newInstance(t)
	host := scanner.Runner()[:strings.LastIndexByte(scanner.Runner(), '/')]
	storetest.Exec(t, in.st, `INSERT INTO com_nalet_katalog_scanjobs (id, source, status, startedat, runner, heartbeatat) VALUES
		('restarted', 'nfs', 'running', localtimestamp - interval '3 minutes', $1, now() - interval '1 minute'),
		('stopped', 'nfs', 'running', localtimestamp - interval '2 hours', 'elsewhere/00000000', now() - interval '1 hour'),
		('walking', 'nfs', 'running', localtimestamp - interval '2 hours', 'elsewhere/11111111', now() - interval '10 seconds')`,
		host+"/00000000")
	ctx := context.Background()
	if n, err := scanner.New(in.st, config.Config{}, processing.New(in.st.Pool()), nil).FailInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("at startup: %d failed, %v; want the restarted scan", n, err)
	}
	if n, err := retry.New(in.st, processing.DefaultPolicy(), nil, time.Minute).ReapScans(ctx); err != nil || n != 1 {
		t.Fatalf("the reaper: %d failed, %v; want the stopped scan", n, err)
	}

	doc := `{ restarted: scanJob(id: "restarted") { status errorMessage } stopped: scanJob(id: "stopped") { status errorMessage }
		walking: scanJob(id: "walking") { status errorMessage } }`
	if _, a := in.gql(t, "/api/manage/query", in.iss.Viewer(t), doc); len(a.Errors) == 0 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
		t.Errorf("a viewer: %s %v, want it refused", a.Data, a.Errors)
	}
	_, a := in.gql(t, "/api/manage/query", in.iss.Admin(t), doc)
	want := `{"restarted":{"status":"failed","errorMessage":"interrupted: the service restarted while the scan ran"},` +
		`"stopped":{"status":"failed","errorMessage":"timed out: no word from its scanner for 15m (the scan's timeout)"},` +
		`"walking":{"status":"running","errorMessage":null}}`
	if len(a.Errors) > 0 || string(a.Data) != want {
		t.Errorf("an admin's scan jobs:\n got  %s %v\n want %s", a.Data, a.Errors, want)
	}
	if _, a := in.gql(t, "/api/manage/query", in.iss.Service(t, "zaentrum-manager"), `mutation { triggerScan { status } }`); len(a.Errors) > 0 ||
		string(a.Data) != `{"triggerScan":{"status":"running"}}` {
		t.Errorf("the scan Job's scan: %s %v", a.Data, a.Errors)
	}
}
