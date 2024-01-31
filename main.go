package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alitto/pond"
	"github.com/dustin/go-humanize"
	"github.com/google/go-github/v58/github"
	"github.com/nkall/compactnumber"
)

type Repository struct {
	c *github.Client

	stats     map[string]*Stats
	numRepos  int64
	reposDone int64
	lock      *sync.Mutex
}

type Contributions struct {
	Additions int64
	Deletions int64
	Commits   int64
}

type Stats struct {
	Totals         *Contributions
	Week           *Contributions
	Month          *Contributions
	Year           *Contributions
	Contributor    *github.Contributor
	EarliestCommit time.Time
	LastCommit     time.Time
}

func (s *Stats) PerDay() float64 {
	return float64(s.Totals.Commits) / s.LastCommit.Sub(s.EarliestCommit).Hours() * 24
}

func (s *Stats) PerWeek() float64 {
	return float64(s.Totals.Commits) / s.LastCommit.Sub(s.EarliestCommit).Hours() * 168
}

func (s *Stats) PerMonth() float64 {
	return float64(s.Totals.Commits) / s.LastCommit.Sub(s.EarliestCommit).Hours() * 720
}

func (s *Stats) PerYear() float64 {
	return float64(s.Totals.Commits) / s.LastCommit.Sub(s.EarliestCommit).Hours() * 8760
}

func main() {
	// Read organization, token and output file from command line
	organization := flag.String("organization", "", "The organization to get stats for")
	token := flag.String("token", "", "The GitHub token to use")
	outputFile := flag.String("output", "output.html", "The file to output the stats to")
	flag.Parse()

	newpath := filepath.Join(".", *outputFile)
	err := os.MkdirAll(newpath, os.ModePerm)
	if err != nil {
		panic(err)
	}

	if *organization == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *token == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	r := &Repository{
		c:     github.NewTokenClient(context.Background(), *token),
		stats: make(map[string]*Stats),
		lock:  &sync.Mutex{},
	}

	err = r.getOrganizationStats(*organization)
	if err != nil {
		panic(err)
	}

	formatter := compactnumber.NewFormatter("en-US", compactnumber.Short)

	// Parse template, execute and output it
	tmpl, err := template.New("stats.html").
		Funcs(template.FuncMap{
			"humanize": humanize.Comma,
			"mod":      func(i, j int) bool { return i%j == 0 },
			"format": func(n int64) string {
				str, err := formatter.Format(int(n))
				if err != nil {
					panic(err)
				}
				return str
			},
			"float": func(f float64) string {
				return fmt.Sprintf("%.2f", f)
			},
		}).
		ParseFiles("stats.html")
	if err != nil {
		panic(err)
	}

	w := bytes.NewBuffer([]byte{})
	err = tmpl.Execute(w, map[string]interface{}{
		"Stats":        r.stats,
		"Organization": organization,
	})
	if err != nil {
		panic(err)
	}

	// Write output to a HTML file
	err = os.WriteFile(*outputFile, w.Bytes(), 0644)
	if err != nil {
		panic(err)
	}
}

func (r *Repository) getOrganizationStats(name string) error {
	nextPage := 1
	repos := []*github.Repository{}

	for nextPage > 0 {
		log.Println("Getting page:", nextPage, " of organization:", name)

		rs, resp, err := r.c.Repositories.ListByOrg(context.Background(), name, &github.RepositoryListByOrgOptions{
			ListOptions: github.ListOptions{PerPage: 100, Page: nextPage},
		})
		if err != nil {
			return err
		}

		nextPage = resp.NextPage
		repos = append(repos, rs...)
	}

	r.numRepos = int64(len(repos))

	pool := pond.New(30, 0, pond.MinWorkers(10))

	for _, repo := range repos {
		func(rep *github.Repository) {
			pool.Submit(func() {
				err := r.getRepositoryStats(rep)
				if err != nil {
					log.Fatalln(err)
				}
			})
		}(repo)
	}

	pool.StopAndWait()
	return nil
}

func (r *Repository) getRepositoryStats(repo *github.Repository) error {
	log.Print("Getting stats for repository: ", *repo.Name)

	stats := []*github.ContributorStats{}
	resp := &github.Response{
		Response: &http.Response{
			StatusCode: 202,
		},
	}

	for resp.StatusCode == 202 || resp.StatusCode == 403 {
		var err error
		stats, resp, err = r.c.Repositories.ListContributorsStats(context.Background(), *repo.Owner.Login, *repo.Name)
		if err != nil && resp.StatusCode != 202 && resp.StatusCode != 403 {
			return err
		}

		if resp.StatusCode == 202 {
			time.Sleep(time.Second * 2)
		}

		if resp.StatusCode == 403 {
			log.Println("Rate limited, waiting 10 seconds on repository:", *repo.Name)
			time.Sleep(time.Second * 10)
		}
	}

	if resp.StatusCode != 200 {
		if resp.StatusCode == 204 {
			log.Printf("No stats for repository: %s\n", *repo.Name)
			return nil
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("Failed to read failure reason: %s", *repo.Name)
		}

		return fmt.Errorf("Failed to get stats for repository: %s (%d): %s", *repo.Name, resp.StatusCode, data)
	}

	for _, s := range stats {
		r.setStats(s)
	}

	r.reposDone++

	log.Printf(
		"Got stats for repository: %s (%d/%d) (%.2f%%)\n",
		*repo.Name,
		r.reposDone,
		r.numRepos,
		float64(r.reposDone)/float64(r.numRepos)*100.0,
	)
	return nil
}

func (r *Repository) setStats(s *github.ContributorStats) {
	r.lock.Lock()
	defer r.lock.Unlock()
	stats := r.stats[*s.Author.Login]
	if stats == nil {
		stats = &Stats{
			Contributor: s.Author,
			Totals:      &Contributions{},
			Week:        &Contributions{},
			Month:       &Contributions{},
			Year:        &Contributions{},
		}

		r.stats[*s.Author.Login] = stats
	}

	// Go through each week and add it to the stats
	for _, w := range s.Weeks {
		stats.Totals.Additions += int64(*w.Additions)
		stats.Totals.Deletions += int64(*w.Deletions)
		stats.Totals.Commits += int64(*w.Commits)

		if time.Now().Sub(w.Week.Time).Hours() < 168 {
			stats.Week.Additions += int64(*w.Additions)
			stats.Week.Deletions += int64(*w.Deletions)
			stats.Week.Commits += int64(*w.Commits)
		}

		if time.Now().Sub(w.Week.Time).Hours() < 720 {
			stats.Month.Additions += int64(*w.Additions)
			stats.Month.Deletions += int64(*w.Deletions)
			stats.Month.Commits += int64(*w.Commits)
		}

		if time.Now().Sub(w.Week.Time).Hours() < 8760 {
			stats.Year.Additions += int64(*w.Additions)
			stats.Year.Deletions += int64(*w.Deletions)
			stats.Year.Commits += int64(*w.Commits)
		}

		if stats.EarliestCommit.IsZero() || w.Week.Time.Before(stats.EarliestCommit) {
			stats.EarliestCommit = w.Week.Time
		}

		if stats.LastCommit.IsZero() || w.Week.Time.After(stats.LastCommit) {
			stats.LastCommit = w.Week.Time
		}
	}
}
