//  Copyright (c) 2015 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package cbft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	"github.com/couchbase/cbauth"
	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/rest"
)

// Map of "method:path" => "perm".  For example, "GET:/api/index" =>
// "cluster.bucket.fts!read".
var restPermsMap = map[string]string{}
var restAuditMap = map[string]uint32{}

func init() {
	// Initialze restPermsMap from restPerms.
	rps := strings.Split(strings.TrimSpace(restPerms), "\n\n")
	for _, rp := range rps {
		// Example rp: "GET /api/index\ncluster.bucket...!read".
		rpa := strings.Split(rp, "\n")
		ra := strings.Split(rpa[0], " ")
		method := ra[0]
		path := ra[1]
		perm := rpa[1]
		restPermsMap[method+":"+path] = perm
		if len(rpa) > 2 {
			eventId, _ := strconv.ParseUint(rpa[2], 0, 32)
			restAuditMap[method+":"+path] = uint32(eventId)
		}
	}
}

// --------------------------------------------------------

func CheckAPIAuth(mgr *cbgt.Manager,
	w http.ResponseWriter, req *http.Request, path string) (allowed bool) {
	authType := ""
	if mgr != nil && mgr.Options() != nil {
		authType = mgr.Options()["authType"]
	}

	if authType == "" {
		return true
	}

	if authType != "cbauth" {
		return false
	}

	creds, err := cbauth.AuthWebCreds(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("rest_auth: cbauth.AuthWebCreds,"+
			" err: %v ", err), 403)
		return false
	}

	perms, err := preparePerms(mgr, req, req.Method, path)
	if err != nil {
		http.Error(w, fmt.Sprintf("rest_auth: preparePerm,"+
			" err: %v ", err), 403)
		return false
	}

	for _, perm := range perms {
		allowed, err = creds.IsAllowed(perm)
		if err != nil {
			http.Error(w, fmt.Sprintf("rest_auth: cbauth.IsAllowed,"+
				" err: %v ", err), 403)
			return false
		}

		if !allowed {
			cbauth.SendForbidden(w, perm)
			return false
		}
	}

	return true
}

func UrlWithAuth(authType, urlStr string) (string, error) {
	if authType == "cbauth" {
		return cbgt.CBAuthURL(urlStr)
	}

	return urlStr, nil
}

// --------------------------------------------------------

func sourceNamesForAlias(name string, indexDefsByName map[string]*cbgt.IndexDef,
	depth int) ([]string, error) {
	if depth > 50 {
		return nil, errAliasExpanstionTooDeep
	}
	var rv []string
	indexDef, exists := indexDefsByName[name]
	if exists && indexDef != nil && indexDef.Type == "fulltext-alias" {
		aliasParams, err := parseAliasParams(indexDef.Params)
		if err != nil {
			return nil, fmt.Errorf("error expanding fulltext-alias: %v", err)
		}
		for aliasIndexName := range aliasParams.Targets {
			aliasIndexDef, exists := indexDefsByName[aliasIndexName]
			// if alias target doesn't exist, do nothing
			if exists {
				if aliasIndexDef.Type == "fulltext-alias" {
					// handle nested aliases with recursive call
					nestedSources, err := sourceNamesForAlias(aliasIndexName,
						indexDefsByName, depth+1)
					if err != nil {
						return nil, err
					}
					rv = append(rv, nestedSources...)
				} else {
					rv = append(rv, aliasIndexDef.SourceName)
				}
			}
		}
	}

	return rv, nil
}

// an interface to abstract the bare minimum aspect of a cbgt.Manager
// that we need, so that we can stub the interface for testing
type definitionLookuper interface {
	GetPIndex(pindexName string) *cbgt.PIndex
	GetIndexDefs(refresh bool) (*cbgt.IndexDefs, map[string]*cbgt.IndexDef, error)
}

var errIndexNotFound = fmt.Errorf("index not found")
var errPIndexNotFound = fmt.Errorf("pindex not found")
var errAliasExpanstionTooDeep = fmt.Errorf("alias expansion too deep")

func sourceNamesFromReq(mgr definitionLookuper, req *http.Request,
	method, path string) ([]string, error) {
	sourceNames := make([]string, 0)
	indexName := rest.IndexNameLookup(req)
	if indexName != "" {
		_, indexDefsByName, err := mgr.GetIndexDefs(false)
		if err != nil {
			return nil, err
		}

		indexDef, exists := indexDefsByName[indexName]
		if !exists || indexDef == nil {
			if method == "PUT" {
				// Special case where PUT can mean CREATE, which
				// we assume when there's no indexDef.
				return sourceNamesFromReq(mgr, req, "CREATE", path)
			} else if method == "CREATE" {
				sourceNames, err = findCouchbaseSourceNames(req, indexName, indexDefsByName)
				if err != nil {
					return nil, err
				}
				return sourceNames, nil
			} else {
				return nil, errIndexNotFound
			}
		}

		if indexDef.Type == "fulltext-alias" {
			// this finds the sources in current definition
			currSourceNames, err := sourceNamesForAlias(indexName, indexDefsByName, 0)
			if err != nil {
				return nil, err
			}
			sourceNames = append(sourceNames, currSourceNames...)
			// now also extra sources from new definition in the request
			newSourceNames, err := findCouchbaseSourceNames(req, indexName, indexDefsByName)
			if err != nil {
				return nil, err
			}
			sourceNames = append(sourceNames, newSourceNames...)
		} else {
			// first use the source in current definition
			sourceNames = append(sourceNames, indexDef.SourceName)
			// now also add any sources from new definition in the request
			newSourceNames, err := findCouchbaseSourceNames(req, indexName, indexDefsByName)
			if err != nil {
				return nil, err
			}
			sourceNames = append(sourceNames, newSourceNames...)
		}
	} else {
		pindexName := rest.PIndexNameLookup(req)
		if pindexName != "" {
			pindex := mgr.GetPIndex(pindexName)
			if pindex == nil {
				return nil, errPIndexNotFound
			}
			sourceNames = append(sourceNames, pindex.SourceName)
		} else {
			return nil, fmt.Errorf("missing indexName/pindexName")
		}
	}

	return sourceNames, nil
}

func preparePerms(mgr definitionLookuper, req *http.Request,
	method, path string) ([]string, error) {

	perm := restPermsMap[method+":"+path]
	if perm == "" {
		perm = restPermDefault
	} else if perm == "none" {
		return nil, nil
	}

	if strings.Index(perm, "<sourceName>") >= 0 {
		sourceNames, err := sourceNamesFromReq(mgr, req, method, path)
		if err != nil {
			return nil, err
		}

		if len(sourceNames) > 0 {
			// found some source names to replace
			perms := make([]string, 0, len(sourceNames))
			for _, sourceName := range sourceNames {
				perms = append(perms,
					strings.Replace(perm, "<sourceName>", sourceName, -1))
			}
			return perms, nil
		} else {
			// perm depends on sources, there are no sources, therefore no perms
			return []string{}, nil
		}
	}

	return []string{perm}, nil
}

func findCouchbaseSourceNames(req *http.Request, indexName string,
	indexDefsByName map[string]*cbgt.IndexDef) (rv []string, err error) {
	var requestBody []byte

	if req.Body != nil {
		requestBody, err = ioutil.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}

	// reset req.Body so it can be read later by the handler
	req.Body = ioutil.NopCloser(bytes.NewReader(requestBody))

	var indexDef cbgt.IndexDef
	if len(requestBody) > 0 {
		err := json.Unmarshal(requestBody, &indexDef)
		if err != nil {
			return nil, err
		}
	}

	if indexDef.Type == "fulltext-index" {
		sourceType, sourceName := rest.ExtractSourceTypeName(req, &indexDef, indexName)
		if sourceType == "couchbase" {
			return []string{sourceName}, nil
		}
	} else if indexDef.Type == "fulltext-alias" {
		// create a copy of indexDefNames with the new one added
		futureIndexDefsByName := make(map[string]*cbgt.IndexDef,
			len(indexDefsByName)+1)
		for k, v := range indexDefsByName {
			futureIndexDefsByName[k] = v
		}
		futureIndexDefsByName[indexName] = &indexDef
		aliasSourceNames, err := sourceNamesForAlias(indexName, futureIndexDefsByName, 0)
		if err != nil {
			return nil, err
		}
		return aliasSourceNames, nil
	}
	return nil, nil
}

type CBAuthBasicLogin struct {
	mgr *cbgt.Manager
}

func CBAuthBasicLoginHandler(mgr *cbgt.Manager) (*CBAuthBasicLogin, error) {
	return &CBAuthBasicLogin{
		mgr: mgr,
	}, nil
}

func (h *CBAuthBasicLogin) ServeHTTP(
	w http.ResponseWriter, req *http.Request) {

	authType := ""
	if h.mgr != nil && h.mgr.Options() != nil {
		authType = h.mgr.Options()["authType"]
	}

	if authType == "cbauth" {
		creds, err := cbauth.AuthWebCreds(req)
		if err != nil {
			http.Error(w, fmt.Sprintf("rest_auth: cbauth.AuthWebCreds,"+
				" err: %v ", err), 403)
			return
		}

		if creds.Source() == "anonymous" {
			// force basic auth login by sending 401
			cbauth.SendUnauthorized(w)
			return
		}
	}

	// redirect to /
	http.Redirect(w, req, "/", http.StatusMovedPermanently)
}
