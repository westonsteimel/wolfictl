package advisory

import (
	"fmt"
	"sort"

	"github.com/wolfi-dev/wolfictl/pkg/configs"
	v2 "github.com/wolfi-dev/wolfictl/pkg/configs/advisory/v2"
)

// UpdateOptions configures the Update operation.
type UpdateOptions struct {
	// AdvisoryDocs is the Index of advisory documents on which to operate.
	AdvisoryDocs *configs.Index[v2.Document]
}

// Update adds a new event to an existing advisory (named by the vuln parameter)
// in the document at the provided path.
func Update(req Request, opts UpdateOptions) error {
	vulnID := req.VulnerabilityID

	documents := opts.AdvisoryDocs.Select().WhereName(req.Package)
	if count := documents.Len(); count != 1 {
		return fmt.Errorf("cannot update advisory: found %d advisory documents for package %q", count, req.Package)
	}

	u := v2.NewAdvisoriesSectionUpdater(func(doc v2.Document) (v2.Advisories, error) {
		advisories := doc.Advisories

		adv, ok := advisories.Get(vulnID)
		if !ok {
			return nil, fmt.Errorf("advisory %q does not exist", vulnID)
		}

		adv.Events = append(adv.Events, req.Event)
		advisories = advisories.Update(vulnID, adv)

		// Ensure the package's advisory list is sorted before returning it.
		sort.Sort(advisories)

		return advisories, nil
	})
	err := documents.Update(u)
	if err != nil {
		return fmt.Errorf("unable to add entry for advisory %q in %q: %w", vulnID, req.Package, err)
	}

	// Update the schema version to the latest version.
	err = documents.Update(v2.NewSchemaVersionSectionUpdater(v2.SchemaVersion))
	if err != nil {
		return fmt.Errorf("unable to update schema version for %q: %w", req.Package, err)
	}

	return nil
}
