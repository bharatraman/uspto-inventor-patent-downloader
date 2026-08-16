package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	odp "github.com/patent-dev/uspto-odp"
)

const (
	searchPageSize      = 100
	minimumRequestDelay = 750 * time.Millisecond
	maximumRetries      = 5
)

var (
	unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	nondigitChars       = regexp.MustCompile(`[^0-9]+`)
	publicationPattern  = regexp.MustCompile(`(?i)^(?:US)?([0-9]{11})(?:[A-Z][0-9]?)?$`)
)

type DocumentResult struct {
	Number      string `json:"number,omitempty"`
	SourceValue string `json:"source_value,omitempty"`
	Status      string `json:"status"`
	DownloadURL string `json:"download_url,omitempty"`
	LocalFile   string `json:"local_file,omitempty"`
	Error       string `json:"error,omitempty"`
}

type ApplicationRecord struct {
	Inventor          string         `json:"inventor"`
	ApplicationNumber string         `json:"application_number"`
	PatentNumber      string         `json:"patent_number,omitempty"`
	PublicationNumber string         `json:"publication_number,omitempty"`
	Title             string         `json:"title,omitempty"`
	FirstInventor     string         `json:"first_inventor,omitempty"`
	GrantDate         string         `json:"grant_date,omitempty"`
	PublicationDate   string         `json:"publication_date,omitempty"`
	Grant             DocumentResult `json:"grant"`
	Publication       DocumentResult `json:"publication"`
}

type Summary struct {
	Inventor                string `json:"inventor"`
	SearchResults           int    `json:"search_results"`
	ApplicationsProcessed   int    `json:"applications_processed"`
	GrantNumbersFound       int    `json:"grant_numbers_found"`
	PublicationNumbersFound int    `json:"publication_numbers_found"`
	GrantsAvailable         int    `json:"grants_available"`
	PublicationsAvailable   int    `json:"publications_available"`
	DownloadsFailed         int    `json:"downloads_failed"`
	OutputDirectory         string `json:"output_directory"`
	ZipFile                 string `json:"zip_file"`
	GeneratedAt             string `json:"generated_at"`
}

type Manifest struct {
	Summary      Summary             `json:"summary"`
	Applications []ApplicationRecord `json:"applications"`
}

type RateLimiter struct {
	mu       sync.Mutex
	lastCall time.Time
	interval time.Duration
}

func NewRateLimiter(interval time.Duration) *RateLimiter {
	return &RateLimiter{interval: interval}
}

func (r *RateLimiter) Wait(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	wait := r.interval - time.Since(r.lastCall)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	r.lastCall = time.Now()
	return nil
}

func str(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func safeName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "_")
	value = unsafeFilenameChars.ReplaceAllString(value, "_")
	value = strings.Trim(value, "._-")
	if value == "" {
		return "unknown"
	}
	return value
}

func escapeQueryValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func numericDocumentNumber(value string) string {
	return nondigitChars.ReplaceAllString(value, "")
}

func publicationDocumentNumber(value string) string {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "/", "")
	normalized = strings.ReplaceAll(normalized, ",", "")
	normalized = strings.ReplaceAll(normalized, "-", "")

	matches := publicationPattern.FindStringSubmatch(normalized)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	minimum := 5 * time.Second
	if resp != nil {
		retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
		if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
			delay := time.Duration(seconds) * time.Second
			if delay < minimum {
				delay = minimum
			}
			return delay + time.Duration(rand.Intn(500))*time.Millisecond
		}
		if retryAfter != "" {
			if retryTime, err := http.ParseTime(retryAfter); err == nil {
				delay := time.Until(retryTime)
				if delay < minimum {
					delay = minimum
				}
				return delay + time.Duration(rand.Intn(500))*time.Millisecond
			}
		}
	}

	delay := minimum * time.Duration(1<<attempt)
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}
	return delay + time.Duration(rand.Intn(500))*time.Millisecond
}

func downloadCompletePatentPDF(
	ctx context.Context,
	httpClient *http.Client,
	limiter *RateLimiter,
	documentNumber string,
	filename string,
) (string, error) {
	url := "https://image-ppubs.uspto.gov/dirsearch-public/print/downloadPdf/" + documentNumber

	for attempt := 0; attempt <= maximumRetries; attempt++ {
		if err := limiter.Wait(ctx); err != nil {
			return url, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return url, err
		}
		req.Header.Set("Accept", "application/pdf")
		req.Header.Set("User-Agent", "Company-USPTO-Patent-PoC/1.0")

		resp, err := httpClient.Do(req)
		if err != nil {
			if attempt == maximumRetries {
				return url, err
			}
			delay := retryDelay(nil, attempt)
			log.Printf("request error for %s; retrying in %s: %v", documentNumber, delay, err)
			if err := sleepContext(ctx, delay); err != nil {
				return url, err
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if attempt == maximumRetries {
				return url, fmt.Errorf("HTTP status %d after %d attempts", resp.StatusCode, attempt+1)
			}
			delay := retryDelay(resp, attempt)
			log.Printf("HTTP %d for %s; retrying in %s", resp.StatusCode, documentNumber, delay)
			if err := sleepContext(ctx, delay); err != nil {
				return url, err
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return url, fmt.Errorf("HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		temporaryFile := filename + ".part"
		file, err := os.Create(temporaryFile)
		if err != nil {
			_ = resp.Body.Close()
			return url, err
		}

		_, copyErr := io.Copy(file, resp.Body)
		bodyCloseErr := resp.Body.Close()
		fileCloseErr := file.Close()
		if copyErr != nil {
			_ = os.Remove(temporaryFile)
			return url, copyErr
		}
		if bodyCloseErr != nil {
			_ = os.Remove(temporaryFile)
			return url, bodyCloseErr
		}
		if fileCloseErr != nil {
			_ = os.Remove(temporaryFile)
			return url, fileCloseErr
		}

		valid, validationErr := isPDF(temporaryFile)
		if validationErr != nil {
			_ = os.Remove(temporaryFile)
			return url, validationErr
		}
		if !valid {
			_ = os.Remove(temporaryFile)
			return url, fmt.Errorf("response for %s was not a PDF", documentNumber)
		}

		if err := os.Rename(temporaryFile, filename); err != nil {
			_ = os.Remove(temporaryFile)
			return url, err
		}
		return url, nil
	}

	return url, fmt.Errorf("download failed for %s", documentNumber)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isPDF(filename string) (bool, error) {
	file, err := os.Open(filename)
	if err != nil {
		return false, err
	}
	defer file.Close()

	header := make([]byte, 5)
	if _, err := io.ReadFull(file, header); err != nil {
		return false, err
	}
	return string(header) == "%PDF-", nil
}

func existingValidPDF(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil || info.Size() == 0 {
		return false
	}
	valid, err := isPDF(filename)
	return err == nil && valid
}

func writeManifest(filename string, manifest Manifest) error {
	manifest.Summary.GeneratedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	temporaryFile := filename + ".part"
	if err := os.WriteFile(temporaryFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(temporaryFile, filename)
}

func createZip(sourceDir, zipFile string) error {
	temporaryZip := zipFile + ".part"
	file, err := os.Create(temporaryZip)
	if err != nil {
		return err
	}

	writer := zip.NewWriter(file)
	walkErr := filepath.Walk(sourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if path == zipFile || path == temporaryZip {
			return nil
		}

		relativePath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relativePath)
		header.Method = zip.Deflate

		entry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}

		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(entry, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})

	writerCloseErr := writer.Close()
	fileCloseErr := file.Close()
	if walkErr != nil {
		_ = os.Remove(temporaryZip)
		return walkErr
	}
	if writerCloseErr != nil {
		_ = os.Remove(temporaryZip)
		return writerCloseErr
	}
	if fileCloseErr != nil {
		_ = os.Remove(temporaryZip)
		return fileCloseErr
	}

	if err := os.Rename(temporaryZip, zipFile); err != nil {
		_ = os.Remove(temporaryZip)
		return err
	}
	return nil
}

func main() {
	inventor := "Bharatwajan Raman"
	if len(os.Args) > 1 {
		inventor = strings.TrimSpace(os.Args[1])
	}
	if inventor == "" {
		log.Fatal("inventor name cannot be empty")
	}

	apiKey := strings.TrimSpace(os.Getenv("USPTO_API_KEY"))
	if apiKey == "" {
		log.Fatal("USPTO_API_KEY is not set")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}

	inventorFolderName := safeName(inventor)
	outputRoot := filepath.Join(homeDir, "patent-data")
	inventorDir := filepath.Join(outputRoot, inventorFolderName)
	packageDir := filepath.Join(inventorDir, "Complete_Patent_Documents")
	grantsDir := filepath.Join(packageDir, "Grants")
	publicationsDir := filepath.Join(packageDir, "Publications")
	manifestPath := filepath.Join(packageDir, "manifest.json")
	zipPath := filepath.Join(inventorDir, inventorFolderName+"_Complete_Patent_Documents.zip")

	if err := os.MkdirAll(grantsDir, 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(publicationsDir, 0755); err != nil {
		log.Fatal(err)
	}

	cfg := odp.DefaultConfig()
	cfg.APIKey = apiKey
	client, err := odp.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	limiter := NewRateLimiter(minimumRequestDelay)
	httpClient := &http.Client{Timeout: 10 * time.Minute}
	query := fmt.Sprintf(
		`applicationMetaData.inventorBag.inventorNameText:"%s"`,
		escapeQueryValue(inventor),
	)

	applicationsByNumber := make(map[string]ApplicationRecord)
	totalSearchResults := 0

	for offset := 0; ; offset += searchPageSize {
		if err := limiter.Wait(ctx); err != nil {
			log.Fatal(err)
		}
		result, err := client.SearchPatents(ctx, query, offset, searchPageSize)
		if err != nil {
			log.Fatalf("SearchPatents offset %d failed: %v", offset, err)
		}
		if result.Count != nil {
			totalSearchResults = *result.Count
		}
		if result.PatentFileWrapperDataBag == nil || len(*result.PatentFileWrapperDataBag) == 0 {
			break
		}

		page := *result.PatentFileWrapperDataBag
		for _, patent := range page {
			applicationNumber := str(patent.ApplicationNumberText)
			if applicationNumber == "" {
				continue
			}

			publicationSource := str(patent.ApplicationMetaData.EarliestPublicationNumber)
			applicationsByNumber[applicationNumber] = ApplicationRecord{
				Inventor:          inventor,
				ApplicationNumber: applicationNumber,
				PatentNumber:      str(patent.ApplicationMetaData.PatentNumber),
				PublicationNumber: publicationSource,
				Title:             str(patent.ApplicationMetaData.InventionTitle),
				FirstInventor:     str(patent.ApplicationMetaData.FirstInventorName),
				GrantDate:         str(patent.ApplicationMetaData.GrantDate),
				PublicationDate:   str(patent.ApplicationMetaData.EarliestPublicationDate),
				Grant: DocumentResult{
					SourceValue: str(patent.ApplicationMetaData.PatentNumber),
					Status:      "not_available",
				},
				Publication: DocumentResult{
					SourceValue: publicationSource,
					Status:      "not_available",
				},
			}
		}

		if len(page) < searchPageSize {
			break
		}
		if totalSearchResults > 0 && offset+len(page) >= totalSearchResults {
			break
		}
	}

	applicationNumbers := make([]string, 0, len(applicationsByNumber))
	for applicationNumber := range applicationsByNumber {
		applicationNumbers = append(applicationNumbers, applicationNumber)
	}
	sort.Strings(applicationNumbers)

	manifest := Manifest{
		Summary: Summary{
			Inventor:              inventor,
			SearchResults:         totalSearchResults,
			ApplicationsProcessed: len(applicationNumbers),
			OutputDirectory:       packageDir,
			ZipFile:               zipPath,
		},
		Applications: make([]ApplicationRecord, 0, len(applicationNumbers)),
	}

	for index, applicationNumber := range applicationNumbers {
		record := applicationsByNumber[applicationNumber]
		fmt.Printf("[%d/%d] Application %s\n", index+1, len(applicationNumbers), applicationNumber)

		grantNumber := numericDocumentNumber(record.PatentNumber)
		if grantNumber != "" {
			manifest.Summary.GrantNumbersFound++
			record.Grant.Number = grantNumber
			filename := filepath.Join(grantsDir, "US"+grantNumber+".pdf")
			record.Grant.LocalFile = filepath.ToSlash(filepath.Join("Grants", filepath.Base(filename)))
			record.Grant.DownloadURL = "https://image-ppubs.uspto.gov/dirsearch-public/print/downloadPdf/" + grantNumber

			if existingValidPDF(filename) {
				record.Grant.Status = "existing"
				manifest.Summary.GrantsAvailable++
				fmt.Printf("  Existing grant US%s\n", grantNumber)
			} else {
				fmt.Printf("  Downloading grant US%s\n", grantNumber)
				url, err := downloadCompletePatentPDF(ctx, httpClient, limiter, grantNumber, filename)
				record.Grant.DownloadURL = url
				if err != nil {
					record.Grant.Status = "failed"
					record.Grant.Error = err.Error()
					manifest.Summary.DownloadsFailed++
					log.Printf("grant download failed for %s: %v", grantNumber, err)
				} else {
					record.Grant.Status = "downloaded"
					manifest.Summary.GrantsAvailable++
				}
			}
		}

		publicationNumber := publicationDocumentNumber(record.PublicationNumber)
		if publicationNumber != "" {
			manifest.Summary.PublicationNumbersFound++
			record.Publication.Number = publicationNumber
			filename := filepath.Join(publicationsDir, "US"+publicationNumber+".pdf")

			if grantNumber != "" {
				record.Publication.Status = "superseded_by_grant"
				record.Publication.LocalFile = ""
				record.Publication.DownloadURL = ""
				record.Publication.Error = ""

				if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
					log.Printf("could not remove superseded publication %s: %v", filename, err)
				}
				fmt.Printf("  Skipping publication US%s because grant US%s is available\n", publicationNumber, grantNumber)
			} else {
				record.Publication.LocalFile = filepath.ToSlash(filepath.Join("Publications", filepath.Base(filename)))
				record.Publication.DownloadURL = "https://image-ppubs.uspto.gov/dirsearch-public/print/downloadPdf/" + publicationNumber

				if existingValidPDF(filename) {
					record.Publication.Status = "existing"
					manifest.Summary.PublicationsAvailable++
					fmt.Printf("  Existing publication US%s\n", publicationNumber)
				} else {
					fmt.Printf("  Downloading publication US%s\n", publicationNumber)
					url, err := downloadCompletePatentPDF(ctx, httpClient, limiter, publicationNumber, filename)
					record.Publication.DownloadURL = url
					if err != nil {
						record.Publication.Status = "failed"
						record.Publication.Error = err.Error()
						manifest.Summary.DownloadsFailed++
						log.Printf("publication download failed for %s: %v", publicationNumber, err)
					} else {
						record.Publication.Status = "downloaded"
						manifest.Summary.PublicationsAvailable++
					}
				}
			}
		}

		manifest.Applications = append(manifest.Applications, record)
		if err := writeManifest(manifestPath, manifest); err != nil {
			log.Fatal(err)
		}
	}

	if err := createZip(packageDir, zipPath); err != nil {
		log.Fatal(err)
	}
	if err := writeManifest(manifestPath, manifest); err != nil {
		log.Fatal(err)
	}

	fmt.Println()
	fmt.Printf("Inventor: %s\n", inventor)
	fmt.Printf("Search results: %d\n", manifest.Summary.SearchResults)
	fmt.Printf("Applications processed: %d\n", manifest.Summary.ApplicationsProcessed)
	fmt.Printf("Grant numbers found: %d\n", manifest.Summary.GrantNumbersFound)
	fmt.Printf("Publication numbers found: %d\n", manifest.Summary.PublicationNumbersFound)
	fmt.Printf("Grant PDFs available: %d\n", manifest.Summary.GrantsAvailable)
	fmt.Printf("Publication PDFs available: %d\n", manifest.Summary.PublicationsAvailable)
	fmt.Printf("Download failures: %d\n", manifest.Summary.DownloadsFailed)
	fmt.Printf("Manifest: %s\n", manifestPath)
	fmt.Printf("ZIP: %s\n", zipPath)
}
