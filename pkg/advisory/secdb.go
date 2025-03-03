package advisory

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/wolfi-dev/wolfictl/pkg/advisory/secdb"
	"github.com/wolfi-dev/wolfictl/pkg/configs"
	v2 "github.com/wolfi-dev/wolfictl/pkg/configs/advisory/v2"
)

const apkURL = "{{urlprefix}}/{{reponame}}/{{arch}}/{{pkg.name}}-{{pkg.ver}}.apk"

// BuildSecurityDatabaseOptions contains the options for building a database.
type BuildSecurityDatabaseOptions struct {
	AdvisoryDocIndices []*configs.Index[v2.Document]

	URLPrefix string
	Archs     []string
	Repo      string
}

var ErrNoPackageSecurityData = errors.New("no package security data found")

// BuildSecurityDatabase builds an Alpine-style security database from the given options.
func BuildSecurityDatabase(opts BuildSecurityDatabaseOptions) ([]byte, error) {
	var packageEntries []secdb.PackageEntry

	for _, index := range opts.AdvisoryDocIndices {
		var indexPackageEntries []secdb.PackageEntry

		for _, doc := range index.Select().Configurations() {
			if len(doc.Advisories) == 0 {
				continue
			}

			secfixes := make(secdb.Secfixes)

			sort.Slice(doc.Advisories, func(i, j int) bool {
				return doc.Advisories[i].ID < doc.Advisories[j].ID
			})

			for _, advisory := range doc.Advisories {
				if len(advisory.Events) == 0 {
					continue
				}

				sortedEvents := advisory.SortedEvents()

				latest := sortedEvents[len(advisory.Events)-1]
				vulnID := advisory.ID // TODO: should there be a .GetCVE() method on Advisory?

				switch latest.Type {
				case v2.EventTypeFixed:
					version := latest.Data.(v2.Fixed).FixedVersion
					secfixes[version] = append(secfixes[version], vulnID)
				case v2.EventTypeFalsePositiveDetermination:
					secfixes[secdb.NAK] = append(secfixes[secdb.NAK], vulnID)
				}
			}

			if len(secfixes) == 0 {
				continue
			}

			pe := secdb.PackageEntry{
				Pkg: secdb.Package{
					Name:     doc.Package.Name,
					Secfixes: secfixes,
				},
			}

			indexPackageEntries = append(indexPackageEntries, pe)
		}

		if len(indexPackageEntries) == 0 {
			// Catch the unexpected case where an advisories directory contains no security data.
			return nil, ErrNoPackageSecurityData
		}

		packageEntries = append(packageEntries, indexPackageEntries...)
	}

	db := secdb.Database{
		APKURL:    apkURL,
		Archs:     opts.Archs,
		Repo:      opts.Repo,
		URLPrefix: opts.URLPrefix,
		Packages:  packageEntries,
	}

	return json.MarshalIndent(db, "", "  ")
}
