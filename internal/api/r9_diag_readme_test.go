// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"strings"
	"testing"
)

// T27 P5: colab.json is in the zip whenever the app has its Colab service,
// set up or not ("configured": false, no credential, no path); the README
// says so instead of "(when Colab is set up)".
func TestDiagnosticsReadmeDescribesColabJSON(t *testing.T) {
	stubAppInfo(t)
	ce := newColabEnv(t)
	res, body := ce.doWith("POST", "/api/app/diagnostics", nil, "", "")
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	colabJSON, ok := files["colab.json"]
	if !ok {
		t.Fatal("no colab.json in the zip")
	}
	if !strings.Contains(strings.Join(strings.Fields(colabJSON), ""), `"configured":false`) {
		t.Fatalf("colab.json of a Colab that is not set up = %s", colabJSON)
	}
	ce.noSecrets("colab.json", []byte(colabJSON))
	readme := strings.Join(strings.Fields(files["README.txt"]), " ")
	if strings.Contains(readme, "(when Colab is set up)") {
		t.Errorf("README says colab.json comes only when Colab is set up:\n%s", files["README.txt"])
	}
	if !strings.Contains(readme, `colab.json the Colab settings and state ("configured": false when Colab is not set up)`) {
		t.Errorf("README does not describe colab.json as it is:\n%s", files["README.txt"])
	}
}
