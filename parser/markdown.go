package parser

import (
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kvnxiao/sort-awesome-lists/github"
	"github.com/kvnxiao/sort-awesome-lists/logging"
	"github.com/kvnxiao/sort-awesome-lists/requests"
)

var (
	regexUrlLine      = regexp.MustCompile(`^\s*([*\-]) \[.*?]\((https*|mailto):`)
	regexSimpleUrl    = regexp.MustCompile(`\((https*://.*?)\)`)
	regexHrefIsGithub = regexp.MustCompile(`href="*(https*://github.com/[^\s"]+)"*`)
	linksToIgnore     = []string{
		"meetup.com",
		"youtube.com",
		"google.com",
		"reddit.com",
		"twitter.com",
		"medium.com",
		"libhunt.com",
	}
)

type Repository struct {
	url       *url.URL
	text      string
	stars     int
	repoURL   string
	separator string
}

type GithubBlock struct {
	start        int
	end          int
	repositories []*Repository
}

type ByStars []*Repository

func (s ByStars) Len() int {
	return len(s)
}

func (s ByStars) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByStars) Less(i, j int) bool {
	ri := s[i]
	rj := s[j]
	if ri.stars == rj.stars {
		// sort ascending on lexicographical string order
		return ri.repoURL < rj.repoURL
	} else {
		// sort descending on stars
		return ri.stars > rj.stars
	}
}

type Markdown struct {
	lines  []string
	blocks []*GithubBlock
}

func ParseMarkdown(url string) *Markdown {
	defer fmt.Println(" Done!")
	logging.Verbose("Retrieving markdown...")

	b, err := ioutil.ReadFile(url)
	if err != nil {
		log.Fatalf("couldn't read response body: %v", err)
	}

	markdownBody := string(b)
	lines := strings.Split(markdownBody, "\n")

	marked := false
	var blocks []*GithubBlock
	var repositories []*Repository
	start := 0
	end := 0
	for i, line := range lines {
		logging.Inlinef("Parsing markdown for potential repository links: %d/%d lines.", i+1, len(lines))
		submatches := regexUrlLine.FindStringSubmatch(line)
		if len(submatches) > 0 {
			separator := submatches[1]
			if !marked {
				marked = true
				start = i
				end = i
			} else {
				end++
			}
			repositories = append(repositories, parseRepoText(line, separator))
		} else {
			if marked {
				blocks = append(blocks, &GithubBlock{
					start:        start,
					end:          end,
					repositories: repositories,
				})
				repositories = nil
			}
			marked = false
		}
	}
	if marked {
		blocks = append(blocks, &GithubBlock{
			start:        start,
			end:          end,
			repositories: repositories,
		})
		repositories = nil
	}
	return &Markdown{
		lines:  lines,
		blocks: blocks,
	}
}

// parseRepoText attempts to parse a line for a github repository url entry
func parseRepoText(line, separator string) *Repository {
	submatch := regexSimpleUrl.FindAllStringSubmatch(line, -1)

	for _, match := range submatch {
		if len(match) < 2 {
			continue
		}

		// check match string without parentheses, to see if it matches a url with http:// or https://
		urlString := match[1]
		u, repoURL := parseURLForGithubAPIEndpoint(urlString)

		// non-empty repo url means we found a github repo
		if repoURL != "" {
			return &Repository{
				text:      line,
				url:       u,
				stars:     0,
				repoURL:   repoURL,
				separator: separator,
			}
		}

		// empty repo url means we haven't found a direct github repo, try retrieving the HTML contents
		githubURL := readHTMLTextForGithubURL(urlString)
		if githubURL != "" {
			u, repoURL := parseURLForGithubAPIEndpoint(githubURL)
			if repoURL != "" {
				return &Repository{
					text:      line,
					url:       u,
					stars:     0,
					repoURL:   repoURL,
					separator: separator,
				}
			}
		}
	}

	// default case for no matches found
	return &Repository{
		text:      line,
		url:       nil,
		stars:     0,
		repoURL:   "",
		separator: separator,
	}
}

// parseURLForGithubAPIEndpoint parses a url string for a potential github repository
// i.e. github.com/USERNAME/REPO_NAME
//  or  USERNAME.github.io/REPO_NAME
func parseURLForGithubAPIEndpoint(urlString string) (*url.URL, string) {
	u, err := url.Parse(urlString)
	if err != nil {
		log.Fatalf("an error occurred parsing url %s for potential repository: %s", urlString, err)
	}

	// parse hostname and path for potential github repo api endpoint
	hostname := u.Hostname()
	path := u.Path
	repoURL := github.GetApiEndpoint(hostname, path)
	return u, repoURL
}

// readHTMLTextForGithubURL fetches the html contents from a url and parses the contents for a potential github link
func readHTMLTextForGithubURL(urlString string) string {
	if filterOutUrl(urlString) {
		return ""
	}

	logging.Verbosef("checking HTML from %s", urlString)
	resp, err := requests.Get(urlString, nil)
	if err != nil {
		logging.Verbosef("a non-fatal error occurred retrieving the HTML for url (%s): %v", urlString, err)
		return ""
	}
	defer resp.Body.Close()

	htmlText, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logging.Verbosef("a non-fatal error occurred reading the HTML for url (%s): %v", urlString, err)
		return ""
	}

	submatch := regexHrefIsGithub.FindStringSubmatch(string(htmlText))
	if len(submatch) < 2 {
		return ""
	}

	return submatch[1]
}

// filterOutUrl ignores known links that would not contain a github link in the html contents
func filterOutUrl(urlString string) bool {
	for _, s := range linksToIgnore {
		if strings.Contains(urlString, s) {
			return true
		}
	}
	return false
}

func (md *Markdown) CountAll() int {
	c := 0
	for _, block := range md.blocks {
		c += len(block.repositories)
	}
	return c
}

func (md *Markdown) FetchStars(token string, subBlockSize int) {
	defer fmt.Println(" Done!")
	blockCount := len(md.blocks)

	for i, githubBlock := range md.blocks {
		logging.Inlinef("Found %d blocks of repositories. Fetching stars for blocks: %d/%d.", blockCount, i+1, blockCount)
		githubBlock.fetchStars(token, i, subBlockSize)
	}
}

func (md *Markdown) Sort() {
	defer fmt.Println(" Done!")
	for blockNum, githubBlock := range md.blocks {
		logging.Verbosef("Sorting block %d", blockNum)
		logging.Inlinef("Sorting blocks by stars: %d/%d.", blockNum+1, len(md.blocks))
		sort.Sort(ByStars(githubBlock.repositories))

		start := githubBlock.start
		for i, repo := range githubBlock.repositories {
			index := start + i
			numStr := strconv.Itoa(repo.stars)
			indexOfFirstSeparator := strings.Index(repo.text, repo.separator+" ")
			md.lines[index] = repo.text[:indexOfFirstSeparator] + repo.separator + " **" + numStr + "** " + repo.text[indexOfFirstSeparator+2:]
		}
	}
}

func (md *Markdown) ToString() string {
	return strings.Join(md.lines, "\n")
}

func (b *GithubBlock) fetchStars(token string, blockNumber int, subBlockSize int) {
	repoCount := len(b.repositories)

	subBlocks := int(math.Ceil(float64(repoCount) / float64(subBlockSize)))

	logging.Verbosef("Started fetching stars for block %d. Splitting into %d sub-blocks of size %d", blockNumber, subBlocks, subBlockSize)

	for i := 0; i < subBlocks; i++ {
		start := i * subBlockSize
		end := int(math.Min(float64((i+1)*subBlockSize), float64(repoCount)))

		var wg sync.WaitGroup
		wg.Add(end - start)

		for index := start; index < end; index++ {
			repository := b.repositories[index]

			go func(repo *Repository) {
				if repo.repoURL != "" {
					repo.stars = github.GetRepoStars(repository.repoURL, token)
				} else {
					repo.stars = 0
				}

				wg.Done()
			}(repository)
		}

		wg.Wait()
	}
	logging.Verbosef("fetching stars for block %d done.", blockNumber)
}
