package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DeployJob represents a single deployment job (locale/theme/area combo)
type DeployJob struct {
	Locale string
	Theme  string
	Area   string
}

// DeployResult tracks the result of a deployment job
type DeployResult struct {
	Job        DeployJob
	FilesCount int64
	Duration   time.Duration
	Error      string
}

// ModuleConfig represents a Magento module.xml structure
type ModuleConfig struct {
	XMLName xml.Name `xml:"config"`
	Module  struct {
		Name string `xml:"name,attr"`
	} `xml:"module"`
}

var (
	magentoRoot = flag.String("root", ".", "Path to Magento root directory")
	locales     = flag.String("locales", "en_US", "Comma-separated locales (e.g., en_US,nl_NL,de_DE)")
	themes      = flag.String("themes", "Vendor/Hyva", "Comma-separated themes (e.g., Vendor/Hyva,Hyva/reset)")
	areas       = flag.String("areas", "frontend", "Comma-separated areas (default: frontend only)")
	jobs        = flag.Int("jobs", 0, "Number of parallel jobs (0 = auto-detect CPU count)")
	strategy    = flag.String("strategy", "quick", "Deployment strategy (quick, standard, compact)")
	force       = flag.Bool("force", false, "Force deployment even if files exist")
	verbose     = flag.Bool("v", false, "Verbose output")
)

func main() {
	flag.Parse()

	numJobs := *jobs
	if numJobs <= 0 {
		numJobs = runtime.NumCPU()
	}

	if *verbose {
		fmt.Printf("Magento Static Content Deployer (Go)\n")
		fmt.Printf("Root: %s\n", *magentoRoot)
		fmt.Printf("Locales: %v\n", parseCSV(*locales))
		fmt.Printf("Themes: %v\n", parseCSV(*themes))
		fmt.Printf("Areas: %v\n", parseCSV(*areas))
		fmt.Printf("Parallel Jobs: %d\n", numJobs)
		fmt.Printf("Strategy: %s\n\n", *strategy)
	}

	start := time.Now()
	results := deployStatic(
		*magentoRoot,
		parseCSV(*locales),
		parseCSV(*themes),
		parseCSV(*areas),
		numJobs,
		*verbose,
	)

	printResults(results, time.Since(start))

	// Check for actual errors (not skipped themes)
	hasErrors := false
	for _, result := range results {
		if result.Error != "" && !strings.Contains(result.Error, "theme not found") {
			hasErrors = true
			break
		}
	}
	if hasErrors {
		os.Exit(1)
	}
}

// deployStatic orchestrates the parallel deployment
func deployStatic(magentoRoot string, locales, themes, areas []string, numJobs int, verbose bool) []DeployResult {
	// Generate deployment version based on current timestamp
	version := fmt.Sprintf("%d", time.Now().Unix())

	// Create deployment jobs
	jobs := createDeployJobs(locales, themes, areas)

	if verbose {
		fmt.Printf("Created %d deployment jobs\n", len(jobs))
		fmt.Printf("Deployment version: %s\n\n", version)
	}

	// Process jobs in parallel
	results := processJobs(magentoRoot, jobs, numJobs, verbose, version)

	// Create deployment version file if any files were deployed
	totalFiles := int64(0)
	for _, result := range results {
		totalFiles += result.FilesCount
	}
	if totalFiles > 0 {
		createDeploymentVersionFile(magentoRoot, version, verbose)
	}

	return results
}

// createDeployJobs generates all combinations of locales/themes/areas to deploy
func createDeployJobs(locales, themes, areas []string) []DeployJob {
	var jobs []DeployJob

	for _, locale := range locales {
		for _, theme := range themes {
			for _, area := range areas {
				jobs = append(jobs, DeployJob{
					Locale: locale,
					Theme:  theme,
					Area:   area,
				})
			}
		}
	}

	return jobs
}

// themeExists checks if a theme can be found
func themeExists(magentoRoot string, area string, themeName string) bool {
	sourceDirs := []string{
		filepath.Join(magentoRoot, "app/design", area, themeName),
		filepath.Join(magentoRoot, getVendorThemePath(area, themeName)),
	}

	for _, dir := range sourceDirs {
		if _, err := os.Stat(dir); err == nil {
			return true
		}
	}
	return false
}

// deployTask wraps a job and result tracking
type deployTask struct {
	job       DeployJob
	resultIdx int
	results   []DeployResult
}

// worker processes deployment jobs
func worker(wg *sync.WaitGroup, jobChan <-chan *deployTask, magentoRoot string, verbose bool, version string) {
	defer wg.Done()

	for task := range jobChan {
		start := time.Now()
		fileCount, err := deployTheme(magentoRoot, task.job, version)

		result := DeployResult{
			Job:        task.job,
			FilesCount: fileCount,
			Duration:   time.Since(start),
		}

		if err != nil {
			// Check if it's a "not found" error - if so, mark as skipped instead of error
			if strings.Contains(err.Error(), "theme directory not found") {
				result.Error = "" // Don't treat as error
				if verbose {
					fmt.Printf("⊘ %s/%s (%s) - theme not found (skipped)\n", task.job.Theme, task.job.Area, task.job.Locale)
				}
			} else {
				result.Error = fmt.Sprintf("%s/%s (%s): %v", task.job.Theme, task.job.Area, task.job.Locale, err)
				if verbose {
					fmt.Printf("✗ %s/%s (%s) - %v\n", task.job.Theme, task.job.Area, task.job.Locale, err)
				}
			}
		} else {
			if verbose {
				fmt.Printf("✓ %s/%s (%s) - %d files - %.1fs\n", task.job.Theme, task.job.Area, task.job.Locale, fileCount, result.Duration.Seconds())
			}
		}

		task.results[task.resultIdx] = result
	}
}

// processJobs executes deployment jobs with parallelization
func processJobs(magentoRoot string, jobs []DeployJob, numJobs int, verbose bool, version string) []DeployResult {
	results := make([]DeployResult, len(jobs))
	jobChan := make(chan *deployTask, numJobs)
	var wg sync.WaitGroup

	// Start worker goroutines
	for i := 0; i < numJobs; i++ {
		wg.Add(1)
		go worker(&wg, jobChan, magentoRoot, verbose, version)
	}

	// Send jobs to channel
	go func() {
		for i := range jobs {
			jobChan <- &deployTask{
				job:       jobs[i],
				resultIdx: i,
				results:   results,
			}
		}
		close(jobChan)
	}()

	wg.Wait()
	return results
}

// deployTheme handles the actual deployment for a theme/locale/area
// For Hyva-based themes, this copies from:
// 1. Theme web directory: app/design/{area}/{vendor}/{theme}/web
// 2. Library files: vendor/mage-os/magento2-base/lib/web/
// 3. Extension view files from multiple locations:
//    - vendor/*/view/{area}/web/
//    - vendor/*/src/view/{area}/web/
//    - vendor/*/view/base/web/
//    - vendor/*/src/view/base/web/
func deployTheme(magentoRoot string, job DeployJob, version string) (int64, error) {
	// Get the theme vendor/name
	parts := strings.Split(job.Theme, "/")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid theme name: %s", job.Theme)
	}

	themeVendor := parts[0]
	themeName := parts[1]

	// Destination directory - deploy to pub/static/ (nginx handles versioning via URL rewriting)
	destDir := filepath.Join(magentoRoot, "pub/static", job.Area, job.Theme, job.Locale)

	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return 0, fmt.Errorf("failed to create destination directory: %w", err)
	}

	var fileCount int64

	// 1. Copy theme web directory (app/design/{area}/{vendor}/{theme}/web)
	themeWebDir := filepath.Join(magentoRoot, "app/design", job.Area, themeVendor, themeName, "web")
	if _, err := os.Stat(themeWebDir); err == nil {
		count, err := copyDirectory(themeWebDir, destDir)
		if err != nil {
			return 0, fmt.Errorf("failed to copy theme directory: %w", err)
		}
		fileCount += count
	}

	// 2. Copy lib files (vendor/mage-os/magento2-base/lib/web/)
	libDir := filepath.Join(magentoRoot, "vendor/mage-os/magento2-base/lib/web")
	if _, err := os.Stat(libDir); err == nil {
		count, err := copyDirectory(libDir, destDir)
		if err != nil {
			return 0, fmt.Errorf("failed to copy library files: %w", err)
		}
		fileCount += count
	}

	// 3. Copy extension view files from all vendors (vendor/*/view/{area}/web/)
	vendorDir := filepath.Join(magentoRoot, "vendor")
	vendorEntries, err := os.ReadDir(vendorDir)
	if err == nil {
		for _, vendorEntry := range vendorEntries {
			if !vendorEntry.IsDir() {
				continue
			}
			vendorName := vendorEntry.Name()

			// Read each package in the vendor
			vendorPath := filepath.Join(vendorDir, vendorName)
			packageEntries, err := os.ReadDir(vendorPath)
			if err != nil {
				continue
			}

			for _, packageEntry := range packageEntries {
				if !packageEntry.IsDir() {
					continue
				}
				packageName := packageEntry.Name()
				packagePath := filepath.Join(vendorPath, packageName)

				// Get module name for this package
				moduleName := getModuleName(packagePath)

				// Check for view/{area}/web/ directory
				extensionWebDir := filepath.Join(packagePath, "view", job.Area, "web")
				if _, err := os.Stat(extensionWebDir); err == nil {
					// Special case: Magento_Email assets deploy without module prefix
					var count int64
					var err error
					if moduleName == "Magento_Email" {
						count, err = copyDirectory(extensionWebDir, destDir)
					} else {
						count, err = copyDirectoryWithModulePrefix(extensionWebDir, destDir, moduleName)
					}
					if err != nil {
						// Log but don't fail on extension file errors
						continue
					}
					fileCount += count
				}

				// Also check src/view/{area}/web/ (for some packages)
				extensionWebDirSrc := filepath.Join(packagePath, "src", "view", job.Area, "web")
				if _, err := os.Stat(extensionWebDirSrc); err == nil {
					// Special case: Magento_Email assets deploy without module prefix
					var count int64
					var err error
					if moduleName == "Magento_Email" {
						count, err = copyDirectory(extensionWebDirSrc, destDir)
					} else {
						count, err = copyDirectoryWithModulePrefix(extensionWebDirSrc, destDir, moduleName)
					}
					if err != nil {
						continue
					}
					fileCount += count
				}

				// Also check view/base/web/ (for shared vendor modules like hyva-themes)
				extensionBaseDir := filepath.Join(packagePath, "view", "base", "web")
				if _, err := os.Stat(extensionBaseDir); err == nil {
					// Special case: Magento_Email assets deploy without module prefix
					var count int64
					var err error
					if moduleName == "Magento_Email" {
						count, err = copyDirectory(extensionBaseDir, destDir)
					} else {
						count, err = copyDirectoryWithModulePrefix(extensionBaseDir, destDir, moduleName)
					}
					if err != nil {
						continue
					}
					fileCount += count
				}

				// Also check src/view/base/web/ (for some packages)
				extensionBaseDirSrc := filepath.Join(packagePath, "src", "view", "base", "web")
				if _, err := os.Stat(extensionBaseDirSrc); err == nil {
					// Special case: Magento_Email assets deploy without module prefix
					var count int64
					var err error
					if moduleName == "Magento_Email" {
						count, err = copyDirectory(extensionBaseDirSrc, destDir)
					} else {
						count, err = copyDirectoryWithModulePrefix(extensionBaseDirSrc, destDir, moduleName)
					}
					if err != nil {
						continue
					}
					fileCount += count
				}

				// Check for src/module-*/view/{area}/web/ (for multi-module packages like elasticsuite)
				srcModulesPath := filepath.Join(packagePath, "src")
				if srcModuleEntries, err := os.ReadDir(srcModulesPath); err == nil {
					for _, srcModuleEntry := range srcModuleEntries {
						if !srcModuleEntry.IsDir() || !strings.HasPrefix(srcModuleEntry.Name(), "module-") {
							continue
						}
						moduleDir := filepath.Join(srcModulesPath, srcModuleEntry.Name())
						moduleWebDir := filepath.Join(moduleDir, "view", job.Area, "web")

						// Get the module name from the submodule's module.xml
						subModuleName := getModuleName(moduleDir)

						if _, err := os.Stat(moduleWebDir); err == nil {
							count, err := copyDirectoryWithModulePrefix(moduleWebDir, destDir, subModuleName)
							if err != nil {
								continue
							}
							fileCount += count
						}

						// Also check view/base/web/
						moduleBaseDir := filepath.Join(moduleDir, "view", "base", "web")
						if _, err := os.Stat(moduleBaseDir); err == nil {
							count, err := copyDirectoryWithModulePrefix(moduleBaseDir, destDir, subModuleName)
							if err != nil {
								continue
							}
							fileCount += count
						}
					}
				}
			}
		}
	}

	if fileCount == 0 {
		return 0, fmt.Errorf("theme directory not found for %s/%s", job.Area, job.Theme)
	}

	return fileCount, nil
}

// copyDirectoryWithModulePrefix copies files with an optional module name prefix in the path
func copyDirectoryWithModulePrefix(src, dst string, modulePrefix string) (int64, error) {
	var fileCount int64

	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Calculate relative path
		relPath, _ := filepath.Rel(src, path)

		// Skip exclusions
		if shouldSkipFile(relPath) {
			return nil
		}

		// Add module prefix to destination path if provided
		if modulePrefix != "" {
			destPath := filepath.Join(dst, modulePrefix, relPath)
			// Create destination subdirectory
			os.MkdirAll(filepath.Dir(destPath), 0755)
			// Skip if destination exists
			if _, err := os.Stat(destPath); err == nil {
				return nil
			}
			// Copy file
			if err := copyFile(path, destPath); err != nil {
				return err
			}
		} else {
			destPath := filepath.Join(dst, relPath)
			// Create destination subdirectory
			os.MkdirAll(filepath.Dir(destPath), 0755)
			// Skip if destination exists
			if _, err := os.Stat(destPath); err == nil {
				return nil
			}
			// Copy file
			if err := copyFile(path, destPath); err != nil {
				return err
			}
		}

		atomic.AddInt64(&fileCount, 1)
		return nil
	})

	return fileCount, err
}

// copyDirectory recursively copies files from src to dst
func copyDirectory(src, dst string) (int64, error) {
	return copyDirectoryWithModulePrefix(src, dst, "")
}

// copyDirectoryWithModulePrefixOld recursively copies files from src to dst (old version kept for reference)
func copyDirectoryOld(src, dst string) (int64, error) {
	var fileCount int64

	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Calculate relative path
		relPath, _ := filepath.Rel(src, path)
		destPath := filepath.Join(dst, relPath)

		// Create destination subdirectory
		os.MkdirAll(filepath.Dir(destPath), 0755)

		// Skip if destination exists (don't overwrite theme files with lib files)
		if _, err := os.Stat(destPath); err == nil {
			return nil
		}

		// Copy file
		if err := copyFile(path, destPath); err != nil {
			return err
		}

		atomic.AddInt64(&fileCount, 1)
		return nil
	})

	return fileCount, err
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

// printResults prints deployment results summary
func printResults(results []DeployResult, totalDuration time.Duration) {
	fmt.Printf("\n%s\n", "─────────────────────────────────────────────────────────")
	fmt.Printf("Deployment Results\n")
	fmt.Printf("%s\n", "─────────────────────────────────────────────────────────")

	successCount := 0
	totalFiles := int64(0)

	for _, result := range results {
		if result.Error == "" {
			successCount++
			totalFiles += result.FilesCount
			fmt.Printf("✓ %s/%s (%s): %d files in %.1fs\n",
				result.Job.Theme, result.Job.Area, result.Job.Locale, result.FilesCount, result.Duration.Seconds())
		} else {
			fmt.Printf("✗ %s\n", result.Error)
		}
	}

	fmt.Printf("%s\n", "─────────────────────────────────────────────────────────")
	fmt.Printf("Total: %d/%d successful | %d files | %.1fs total\n",
		successCount, len(results), totalFiles, totalDuration.Seconds())
	if totalDuration.Seconds() > 0 {
		fmt.Printf("Average: %.1f files/sec\n", float64(totalFiles)/totalDuration.Seconds())
	}
}

// createDeploymentVersionFile creates the required Magento deployment version file
func createDeploymentVersionFile(magentoRoot string, version string, verbose bool) error {
	versionFile := filepath.Join(magentoRoot, "pub/static/deployed_version.txt")

	// Create the file with the version
	err := os.WriteFile(versionFile, []byte(version), 0644)
	if err != nil {
		return fmt.Errorf("failed to create deployment version file: %w", err)
	}

	if verbose {
		fmt.Printf("✓ Created deployment version file: %s\n", version)
	}

	return nil
}

// getVendorThemePath converts a theme name to its vendor package path
// e.g., "Magento/backend" with adminhtml -> "vendor/magento/theme-adminhtml-backend"
// e.g., "Hyva/reset" with frontend -> "vendor/hyva-themes/magento2-hyva-reset"
// e.g., "MageOS/m137-admin-theme" with adminhtml -> "vendor/mage-os/theme-adminhtml-m137"
func getVendorThemePath(area string, themeName string) string {
	parts := strings.Split(themeName, "/")
	if len(parts) != 2 {
		return ""
	}

	vendor := strings.ToLower(parts[0])
	theme := strings.ToLower(parts[1])

	// Special case mappings for known vendor packages
	switch vendor {
	case "magento":
		if area == "adminhtml" {
			return filepath.Join("vendor", vendor, "theme-"+area+"-"+theme)
		}
		return filepath.Join("vendor", vendor, "theme-frontend-"+theme)
	case "hyva":
		return filepath.Join("vendor", "hyva-themes", "magento2-hyva-"+theme, "web")
	case "mage-os", "mageos":
		return filepath.Join("vendor", "mage-os", "theme-"+area+"-"+theme)
	default:
		// Generic fallback for custom vendors
		if area == "adminhtml" {
			return filepath.Join("vendor", vendor, "theme-adminhtml-"+theme)
		}
		return filepath.Join("vendor", vendor, "theme-frontend-"+theme)
	}
}

// getModuleName extracts the module name from a package's module.xml file
func getModuleName(packagePath string) string {
	moduleXmlPath := filepath.Join(packagePath, "etc", "module.xml")
	if _, err := os.Stat(moduleXmlPath); err != nil {
		// Try src/etc/module.xml
		moduleXmlPath = filepath.Join(packagePath, "src", "etc", "module.xml")
		if _, err := os.Stat(moduleXmlPath); err != nil {
			return ""
		}
	}

	data, err := os.ReadFile(moduleXmlPath)
	if err != nil {
		return ""
	}

	var cfg ModuleConfig
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return ""
	}

	return cfg.Module.Name
}

// shouldSkipFile determines if a file should be excluded from deployment
func shouldSkipFile(relPath string) bool {
	// Exclude documentation directories
	if strings.Contains(relPath, "/docs/") || strings.Contains(relPath, "\\docs\\") {
		return true
	}

	// Exclude tailwind source directory
	if strings.HasPrefix(relPath, "tailwind/") || strings.HasPrefix(relPath, "tailwind\\") {
		return true
	}

	return false
}

// Helper functions

func parseCSV(s string) []string {
	if s == "" {
		return []string{}
	}
	var result []string
	for _, item := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
