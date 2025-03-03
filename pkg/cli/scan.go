package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sbomSyft "github.com/anchore/syft/syft/sbom"
	"github.com/charmbracelet/lipgloss"
	"github.com/samber/lo"
	"github.com/savioxavier/termlink"
	"github.com/spf13/cobra"
	"github.com/wolfi-dev/wolfictl/pkg/buildlog"
	"github.com/wolfi-dev/wolfictl/pkg/configs"
	v2 "github.com/wolfi-dev/wolfictl/pkg/configs/advisory/v2"
	rwos "github.com/wolfi-dev/wolfictl/pkg/configs/rwfs/os"
	"github.com/wolfi-dev/wolfictl/pkg/sbom"
	"github.com/wolfi-dev/wolfictl/pkg/scan"
	"golang.org/x/exp/slices"
)

func cmdScan() *cobra.Command {
	p := &scanParams{}
	cmd := &cobra.Command{
		Use:           "scan <path/to/package.apk> ...",
		Short:         "Scan an apk file for vulnerabilities",
		Args:          cobra.MinimumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if p.outputFormat == "" {
				p.outputFormat = outputFormatOutline
			}

			// Validate inputs

			advisoryDocumentIndices := make([]*configs.Index[v2.Document], 0, len(p.advisoriesRepoDirs))
			for _, dir := range p.advisoriesRepoDirs {
				advisoryFsys := rwos.DirFS(dir)
				index, err := v2.NewIndex(advisoryFsys)
				if err != nil {
					return fmt.Errorf("unable to index advisory configs for directory %q: %w", dir, err)
				}

				advisoryDocumentIndices = append(advisoryDocumentIndices, index)
			}

			if !slices.Contains(validOutputFormats, p.outputFormat) {
				return fmt.Errorf(
					"invalid output format %q, must be one of [%s]",
					p.outputFormat,
					strings.Join(validOutputFormats, ", "),
				)
			}

			if p.advisoryFilterSet != "" {
				if !slices.Contains(scan.ValidAdvisoriesSets, p.advisoryFilterSet) {
					return fmt.Errorf(
						"invalid advisory filter set %q, must be one of [%s]",
						p.advisoryFilterSet,
						strings.Join(scan.ValidAdvisoriesSets, ", "),
					)
				}

				if len(p.advisoriesRepoDirs) == 0 {
					return errors.New("advisory-based filtering requested, but no advisories repo dirs were provided")
				}
			}

			if len(p.advisoriesRepoDirs) > 0 && p.advisoryFilterSet == "" {
				return errors.New("advisories repo dir(s) provided, but no advisory filter set was specified (see -f/--advisory-filter)")
			}

			if p.packageBuildLogInput && p.sbomInput {
				return errors.New("cannot specify both -s/--sbom and --build-log")
			}

			// Use either the build log or the args themselves as scan targets

			var scanInputPaths []string
			if p.packageBuildLogInput {
				if len(args) != 1 {
					return fmt.Errorf("must specify exactly one build log file (or a directory that contains a %q build log file)", buildlog.DefaultName)
				}

				var err error
				scanInputPaths, err = resolveInputFilePathsFromBuildLog(args[0])
				if err != nil {
					return fmt.Errorf("failed to resolve scan inputs from build log: %w", err)
				}
			} else {
				scanInputPaths = args
			}

			// Do a scan for each scan target

			var scans []inputScan
			var inputPathsFailingRequireZero []string
			for _, scanInputPath := range scanInputPaths {
				if p.outputFormat == outputFormatOutline {
					fmt.Printf("🔎 Scanning %q\n", scanInputPath)
				}

				inputFile, err := resolveInputFileFromArg(scanInputPath)
				if err != nil {
					return fmt.Errorf("failed to open input file: %w", err)
				}
				defer inputFile.Close()

				scannedInput, err := scanInput(inputFile, p)
				if err != nil {
					return err
				}

				// If requested, filter scan results using advisories

				if set := p.advisoryFilterSet; set != "" {
					findings, err := scan.FilterWithAdvisories(scannedInput.Result, advisoryDocumentIndices, set)
					if err != nil {
						return fmt.Errorf("failed to filter scan results with advisories during scan of %q: %w", scanInputPath, err)
					}

					scannedInput.Result.Findings = findings
				}

				scans = append(scans, *scannedInput)

				// Handle CLI options

				findings := scannedInput.Result.Findings
				if p.outputFormat == outputFormatOutline {
					// Print output immediately

					if len(findings) == 0 {
						fmt.Println("✅ No vulnerabilities found")
					} else {
						tree := newFindingsTree(findings)
						fmt.Println(tree.render())
					}
				}
				if p.requireZeroFindings && len(findings) > 0 {
					// Accumulate the list of failures to be returned at the end, but we still want to complete all scans
					inputPathsFailingRequireZero = append(inputPathsFailingRequireZero, scanInputPath)
				}
			}

			if p.outputFormat == outputFormatJSON {
				enc := json.NewEncoder(os.Stdout)
				err := enc.Encode(scans)
				if err != nil {
					return fmt.Errorf("failed to marshal scans to JSON: %w", err)
				}
			}

			if len(inputPathsFailingRequireZero) > 0 {
				return fmt.Errorf("vulnerabilities found in the following package(s):\n%s", strings.Join(inputPathsFailingRequireZero, "\n"))
			}

			return nil
		},
	}

	p.addFlagsTo(cmd)
	return cmd
}

func scanInput(inputFile *os.File, p *scanParams) (*inputScan, error) {
	inputFileName := inputFile.Name()

	// Get the SBOM of the APK
	var apkSBOM io.Reader
	if p.sbomInput {
		apkSBOM = inputFile
	} else {
		var s *sbomSyft.SBOM
		var err error

		if p.disableSBOMCache {
			s, err = sbom.Generate(inputFileName, inputFile, p.distro)
		} else {
			s, err = sbom.CachedGenerate(inputFileName, inputFile, p.distro)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to generate SBOM: %w", err)
		}

		reader, err := sbom.ToSyftJSON(s)
		if err != nil {
			return nil, fmt.Errorf("failed to convert SBOM to Syft JSON: %w", err)
		}
		apkSBOM = reader
	}

	// Do the vulnerability scan based on the SBOM
	result, err := scan.APKSBOM(apkSBOM, p.localDBFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to scan APK using %q: %w", inputFileName, err)
	}

	is := &inputScan{
		InputFile: inputFileName,
		Result:    result,
	}

	return is, nil
}

// resolveInputFilePathsFromBuildLog takes the given path to a Melange build log
// file (or a directory that contains the build log as a "packages.log" file).
// Once it finds the build log, it parses it, and returns a slice of file paths
// to APKs to be scanned. Each APK path is created with the assumption that the
// APKs are located at "$BASE/packages/$ARCH/$PACKAGE-$VERSION.apk", where $BASE
// is the buildLogPath if it's a directory, or the directory containing the
// buildLogPath if it's a file.
func resolveInputFilePathsFromBuildLog(buildLogPath string) ([]string, error) {
	pathToFileOrDirectory := filepath.Clean(buildLogPath)

	info, err := os.Stat(pathToFileOrDirectory)
	if err != nil {
		return nil, fmt.Errorf("failed to stat build log input: %w", err)
	}

	var pathToFile, packagesBaseDir string
	if info.IsDir() {
		pathToFile = filepath.Join(pathToFileOrDirectory, buildlog.DefaultName)
		packagesBaseDir = pathToFileOrDirectory
	} else {
		pathToFile = pathToFileOrDirectory
		packagesBaseDir = filepath.Dir(pathToFile)
	}

	file, err := os.Open(pathToFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open build log: %w", err)
	}
	defer file.Close()

	buildLogEntries, err := buildlog.Parse(file)
	if err != nil {
		return nil, fmt.Errorf("failed to parse build log: %w", err)
	}

	scanInputs := make([]string, 0, len(buildLogEntries))
	for _, entry := range buildLogEntries {
		apkName := fmt.Sprintf("%s-%s.apk", entry.Package, entry.FullVersion)
		apkPath := filepath.Join(packagesBaseDir, "packages", entry.Arch, apkName)
		scanInputs = append(scanInputs, apkPath)
	}

	return scanInputs, nil
}

// resolveInputFileFromArg figures out how to interpret the given input file path
// to find a file to scan. This input file could be either an APK or an SBOM.
// The objective of this function is to find the file to scan and return a file
// handle to it.
//
// In order, it will:
//
// 1. If the path is "-", read stdin into a temp file and return that.
//
// 2. If the path starts with "https://", download the remote file into a temp file and return that.
//
// 3. Otherwise, open the file at the given path and return that.
func resolveInputFileFromArg(inputFilePath string) (*os.File, error) {
	switch {
	case inputFilePath == "-":
		// Read stdin into a temp file.
		t, err := os.CreateTemp("", "wolfictl-scan-")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file for stdin: %w", err)
		}
		if _, err := io.Copy(t, os.Stdin); err != nil {
			return nil, err
		}
		if err := t.Close(); err != nil {
			return nil, err
		}

		return t, nil

	case strings.HasPrefix(inputFilePath, "https://"):
		// Fetch the remote URL into a temp file.
		t, err := os.CreateTemp("", "wolfictl-scan-")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file for remote: %w", err)
		}
		resp, err := http.Get(inputFilePath) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("failed to download from remote: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			all, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("failed to download from remote (%d): %s", resp.StatusCode, string(all))
		}
		if _, err := io.Copy(t, resp.Body); err != nil {
			return nil, err
		}
		if err := t.Close(); err != nil {
			return nil, err
		}

		return t, nil

	default:
		inputFile, err := os.Open(inputFilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to open input file: %w", err)
		}

		return inputFile, nil
	}
}

type scanParams struct {
	requireZeroFindings  bool
	localDBFilePath      string
	outputFormat         string
	sbomInput            bool
	packageBuildLogInput bool
	distro               string
	advisoryFilterSet    string
	advisoriesRepoDirs   []string
	disableSBOMCache     bool
}

func (p *scanParams) addFlagsTo(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&p.requireZeroFindings, "require-zero", false, "exit 1 if any vulnerabilities are found")
	cmd.Flags().StringVar(&p.localDBFilePath, "local-file-grype-db", "", "import a local grype db file")
	cmd.Flags().StringVarP(&p.outputFormat, "output", "o", "", fmt.Sprintf("output format (%s), defaults to %s", strings.Join(validOutputFormats, "|"), outputFormatOutline))
	cmd.Flags().BoolVarP(&p.sbomInput, "sbom", "s", false, "treat input(s) as SBOM(s) of APK(s) instead of as actual APK(s)")
	cmd.Flags().BoolVar(&p.packageBuildLogInput, "build-log", false, "treat input as a package build log file (or a directory that contains a packages.log file)")
	cmd.Flags().StringVar(&p.distro, "distro", "wolfi", "distro to use during vulnerability matching")
	cmd.Flags().StringVarP(&p.advisoryFilterSet, "advisory-filter", "f", "", fmt.Sprintf("exclude vulnerability matches that are referenced from the specified set of advisories (%s)", strings.Join(scan.ValidAdvisoriesSets, "|")))
	cmd.Flags().StringSliceVarP(&p.advisoriesRepoDirs, "advisories-repo-dir", "a", nil, "local directory for advisory data")
	cmd.Flags().BoolVar(&p.disableSBOMCache, "disable-sbom-cache", false, "don't use the SBOM cache")
}

const (
	outputFormatOutline = "outline"
	outputFormatJSON    = "json"
)

var validOutputFormats = []string{outputFormatOutline, outputFormatJSON}

type inputScan struct {
	InputFile string
	Result    *scan.Result
}

type findingsTree struct {
	findingsByPackageByLocation map[string]map[string][]*scan.Finding
	packagesByID                map[string]scan.Package
}

func newFindingsTree(findings []*scan.Finding) *findingsTree {
	tree := make(map[string]map[string][]*scan.Finding)
	packagesByID := make(map[string]scan.Package)

	for _, f := range findings {
		loc := f.Package.Location
		packageID := f.Package.ID
		packagesByID[packageID] = f.Package

		if _, ok := tree[loc]; !ok {
			tree[loc] = make(map[string][]*scan.Finding)
		}

		tree[loc][packageID] = append(tree[loc][packageID], f)
	}

	return &findingsTree{
		findingsByPackageByLocation: tree,
		packagesByID:                packagesByID,
	}
}

func (t findingsTree) render() string {
	locations := lo.Keys(t.findingsByPackageByLocation)
	sort.Strings(locations)

	var lines []string
	for i, location := range locations {
		var treeStem, verticalLine string
		if i == len(locations)-1 {
			treeStem = "└── "
			verticalLine = " "
		} else {
			treeStem = "├── "
			verticalLine = "│"
		}

		line := treeStem + fmt.Sprintf("📄 %s", location)
		lines = append(lines, line)

		packageIDs := lo.Keys(t.findingsByPackageByLocation[location])
		packages := lo.Map(packageIDs, func(id string, _ int) scan.Package {
			return t.packagesByID[id]
		})

		sort.SliceStable(packages, func(i, j int) bool {
			return packages[i].Name < packages[j].Name
		})

		for _, pkg := range packages {
			line := fmt.Sprintf(
				"%s       📦 %s %s %s",
				verticalLine,
				pkg.Name,
				pkg.Version,
				styleSubtle.Render("("+pkg.Type+")"),
			)
			lines = append(lines, line)

			findings := t.findingsByPackageByLocation[location][pkg.ID]
			sort.SliceStable(findings, func(i, j int) bool {
				return findings[i].Vulnerability.ID < findings[j].Vulnerability.ID
			})

			for _, f := range findings {
				line := fmt.Sprintf(
					"%s           %s %s%s",
					verticalLine,
					renderSeverity(f.Vulnerability.Severity),
					renderVulnerabilityID(f.Vulnerability),
					renderFixedIn(f.Vulnerability),
				)
				lines = append(lines, line)
			}
		}

		lines = append(lines, verticalLine)
	}

	return strings.Join(lines, "\n")
}

func renderSeverity(severity string) string {
	switch severity {
	case "Negligible":
		return styleNegligible.Render(severity)
	case "Low":
		return styleLow.Render(severity)
	case "Medium":
		return styleMedium.Render(severity)
	case "High":
		return styleHigh.Render(severity)
	case "Critical":
		return styleCritical.Render(severity)
	default:
		return severity
	}
}

func renderVulnerabilityID(vuln scan.Vulnerability) string {
	var cveID string

	for _, alias := range vuln.Aliases {
		if strings.HasPrefix(alias, "CVE-") {
			cveID = alias
			break
		}
	}

	if cveID == "" {
		return hyperlinkVulnerabilityID(vuln.ID)
	}

	return fmt.Sprintf(
		"%s %s",
		hyperlinkVulnerabilityID(cveID),

		styleSubtle.Render(hyperlinkVulnerabilityID(vuln.ID)),
	)
}

var termSupportsHyperlinks = termlink.SupportsHyperlinks()

func hyperlinkVulnerabilityID(id string) string {
	if !termSupportsHyperlinks {
		return id
	}

	switch {
	case strings.HasPrefix(id, "CVE-"):
		return termlink.Link(id, fmt.Sprintf("https://nvd.nist.gov/vuln/detail/%s", id))

	case strings.HasPrefix(id, "GHSA-"):
		return termlink.Link(id, fmt.Sprintf("https://github.com/advisories/%s", id))
	}

	return id
}

func renderFixedIn(vuln scan.Vulnerability) string {
	if vuln.FixedVersion == "" {
		return ""
	}

	return fmt.Sprintf(" fixed in %s", vuln.FixedVersion)
}

var (
	styleSubtle = lipgloss.NewStyle().Foreground(lipgloss.Color("#999999"))

	styleNegligible = lipgloss.NewStyle().Foreground(lipgloss.Color("#999999"))
	styleLow        = lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff00"))
	styleMedium     = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffff00"))
	styleHigh       = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff9900"))
	styleCritical   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
)
