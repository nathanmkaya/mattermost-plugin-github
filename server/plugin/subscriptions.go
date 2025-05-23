// Copyright (c) 2018-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v54/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
)

const (
	SubscriptionsKey          = "subscriptions"
	flagExcludeOrgMember      = "exclude-org-member"
	flagIncludeOnlyOrgMembers = "include-only-org-members"
	flagRenderStyle           = "render-style"
	flagFeatures              = "features"
	flagExcludeRepository     = "exclude"
)

type SubscriptionFlags struct {
	ExcludeOrgMembers     bool
	IncludeOnlyOrgMembers bool
	RenderStyle           string
	ExcludeRepository     []string
}

func (s *SubscriptionFlags) AddFlag(flag string, value string) error {
	switch flag {
	case flagExcludeOrgMember:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		s.ExcludeOrgMembers = parsed
	case flagIncludeOnlyOrgMembers:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		s.IncludeOnlyOrgMembers = parsed
	case flagRenderStyle:
		s.RenderStyle = value
	case flagExcludeRepository:
		repos := strings.Split(value, ",")
		for i := range repos {
			repos[i] = strings.TrimSpace(repos[i])
		}
		s.ExcludeRepository = repos
	}

	return nil
}

func (s SubscriptionFlags) String() string {
	flags := []string{}

	if s.ExcludeOrgMembers {
		flag := "--" + flagExcludeOrgMember + " true"
		flags = append(flags, flag)
	}

	if s.IncludeOnlyOrgMembers {
		flag := "--" + flagIncludeOnlyOrgMembers + " true"
		flags = append(flags, flag)
	}

	if s.RenderStyle != "" {
		flag := "--" + flagRenderStyle + " " + s.RenderStyle
		flags = append(flags, flag)
	}

	if len(s.ExcludeRepository) > 0 {
		flag := "--" + flagExcludeRepository + " " + strings.Join(s.ExcludeRepository, ",")
		flags = append(flags, flag)
	}

	return strings.Join(flags, ",")
}

type Subscription struct {
	ChannelID  string
	CreatorID  string
	Features   Features
	Flags      SubscriptionFlags
	Repository string
}

type Subscriptions struct {
	Repositories map[string][]*Subscription
}

func (s *Subscription) Pulls() bool {
	return strings.Contains(s.Features.String(), featurePulls)
}

func (s *Subscription) PullsCreated() bool {
	return strings.Contains(s.Features.String(), featurePullsCreated)
}

func (s *Subscription) PullsMerged() bool {
	return strings.Contains(s.Features.String(), "pulls_merged")
}

func (s *Subscription) IssueCreations() bool {
	return strings.Contains(s.Features.String(), "issue_creations")
}

func (s *Subscription) Issues() bool {
	return strings.Contains(s.Features.String(), featureIssues)
}

func (s *Subscription) Pushes() bool {
	return strings.Contains(s.Features.String(), "pushes")
}

func (s *Subscription) Creates() bool {
	return strings.Contains(s.Features.String(), "creates")
}

func (s *Subscription) Deletes() bool {
	return strings.Contains(s.Features.String(), "deletes")
}

func (s *Subscription) IssueComments() bool {
	return strings.Contains(s.Features.String(), "issue_comments")
}

func (s *Subscription) PullReviews() bool {
	return strings.Contains(s.Features.String(), "pull_reviews")
}

func (s *Subscription) Stars() bool {
	return strings.Contains(s.Features.String(), featureStars)
}

func (s *Subscription) Workflows() bool {
	return strings.Contains(s.Features.String(), featureWorkflowFailure) || strings.Contains(s.Features.String(), featureWorkflowSuccess)
}

func (s *Subscription) Release() bool {
	return strings.Contains(s.Features.String(), featureReleases)
}

func (s *Subscription) Discussions() bool {
	return strings.Contains(s.Features.String(), featureDiscussions)
}

func (s *Subscription) DiscussionComments() bool {
	return strings.Contains(s.Features.String(), featureDiscussionComments)
}

func (s *Subscription) Label() string {
	if !strings.Contains(s.Features.String(), "label:") {
		return ""
	}

	labelSplit := strings.Split(s.Features.String(), "\"")
	if len(labelSplit) < 3 {
		return ""
	}

	return labelSplit[1]
}

func (s *Subscription) ExcludeOrgMembers() bool {
	return s.Flags.ExcludeOrgMembers
}

func (s *Subscription) IncludeOnlyOrgMembers() bool {
	return s.Flags.IncludeOnlyOrgMembers
}

func (s *Subscription) RenderStyle() string {
	return s.Flags.RenderStyle
}

func (s *Subscription) excludedRepoForSub(repo *github.Repository) bool {
	for _, repository := range s.Flags.ExcludeRepository {
		if repository == repo.GetFullName() {
			return true
		}
	}
	return false
}

func (p *Plugin) Subscribe(ctx context.Context, githubClient *github.Client, userID, owner, repo, channelID string, features Features, flags SubscriptionFlags) error {
	if owner == "" {
		return errors.Errorf("invalid repository")
	}

	owner = strings.ToLower(owner)
	repo = strings.ToLower(repo)

	if err := p.checkOrg(owner); err != nil {
		return errors.Wrap(err, "organization not supported")
	}

	if flags.ExcludeOrgMembers && !p.isOrganizationLocked() {
		return errors.New("Unable to set --exclude-org-member flag. The GitHub plugin is not locked to a single organization.")
	}

	if flags.IncludeOnlyOrgMembers && !p.isOrganizationLocked() {
		return errors.New("Unable to set --include-only-org-members flag. The GitHub plugin is not locked to a single organization.")
	}

	var err, cErr error

	if repo == "" {
		var ghOrg *github.Organization
		cErr = p.useGitHubClient(&GitHubUserInfo{UserID: userID}, func(info *GitHubUserInfo, token *oauth2.Token) error {
			ghOrg, _, err = githubClient.Organizations.Get(ctx, owner)
			if err != nil {
				return err
			}
			return nil
		})
		if ghOrg == nil {
			var ghUser *github.User
			ghUser, _, err = githubClient.Users.Get(ctx, owner)
			if ghUser == nil {
				return errors.Errorf("Unknown organization %s", owner)
			}
		}
	} else {
		var ghRepo *github.Repository
		cErr = p.useGitHubClient(&GitHubUserInfo{UserID: userID}, func(info *GitHubUserInfo, token *oauth2.Token) error {
			ghRepo, _, err = githubClient.Repositories.Get(ctx, owner, repo)
			if err != nil {
				return err
			}
			return nil
		})

		if ghRepo == nil {
			return errors.Errorf("unknown repository %s", fullNameFromOwnerAndRepo(owner, repo))
		}
	}

	if cErr != nil {
		p.client.Log.Warn("Failed to get repository or org for subscribe action", "error", err.Error())
		return errors.Errorf("Encountered an error subscribing to %s", fullNameFromOwnerAndRepo(owner, repo))
	}

	sub := &Subscription{
		ChannelID:  channelID,
		CreatorID:  userID,
		Features:   features,
		Repository: fullNameFromOwnerAndRepo(owner, repo),
		Flags:      flags,
	}

	if err := p.AddSubscription(fullNameFromOwnerAndRepo(owner, repo), sub); err != nil {
		return errors.Wrap(err, "could not add subscription")
	}

	return nil
}

func (p *Plugin) SubscribeOrg(ctx context.Context, githubClient *github.Client, userID, org, channelID string, features Features, flags SubscriptionFlags) error {
	if org == "" {
		return errors.New("invalid organization")
	}

	return p.Subscribe(ctx, githubClient, userID, org, "", channelID, features, flags)
}

func (p *Plugin) GetSubscriptionsByChannel(channelID string) ([]*Subscription, error) {
	var filteredSubs []*Subscription
	subs, err := p.GetSubscriptions()
	if err != nil {
		return nil, errors.Wrap(err, "could not get subscriptions")
	}

	for repo, v := range subs.Repositories {
		for _, s := range v {
			if s.ChannelID == channelID {
				// this is needed to be backwards compatible
				if len(s.Repository) == 0 {
					s.Repository = repo
				}
				filteredSubs = append(filteredSubs, s)
			}
		}
	}

	sort.Slice(filteredSubs, func(i, j int) bool {
		return filteredSubs[i].Repository < filteredSubs[j].Repository
	})

	return filteredSubs, nil
}

func (p *Plugin) AddSubscription(repo string, sub *Subscription) error {
	subs, err := p.GetSubscriptions()
	if err != nil {
		return errors.Wrap(err, "could not get subscriptions")
	}

	repoSubs := subs.Repositories[repo]
	if repoSubs == nil {
		repoSubs = []*Subscription{sub}
	} else {
		exists := false
		for index, s := range repoSubs {
			if s.ChannelID == sub.ChannelID {
				repoSubs[index] = sub
				exists = true
				break
			}
		}

		if !exists {
			repoSubs = append(repoSubs, sub)
		}
	}

	subs.Repositories[repo] = repoSubs

	err = p.StoreSubscriptions(subs)
	if err != nil {
		return errors.Wrap(err, "could not store subscriptions")
	}

	return nil
}

func (p *Plugin) GetSubscriptions() (*Subscriptions, error) {
	var subscriptions *Subscriptions

	err := p.store.Get(SubscriptionsKey, &subscriptions)
	if err != nil {
		return nil, errors.Wrap(err, "could not get subscriptions from KVStore")
	}

	// No subscriptions stored.
	if subscriptions == nil {
		return &Subscriptions{Repositories: map[string][]*Subscription{}}, nil
	}

	return subscriptions, nil
}

func (p *Plugin) StoreSubscriptions(s *Subscriptions) error {
	return p.store.SetAtomicWithRetries(SubscriptionsKey, func(_ []byte) (interface{}, error) {
		modifiedBytes, err := json.Marshal(s)
		if err != nil {
			return nil, errors.Wrap(err, "could not store subscriptions in KV store")
		}

		return modifiedBytes, nil
	})
}

func (p *Plugin) GetSubscribedChannelsForRepository(repo *github.Repository) []*Subscription {
	name := repo.GetFullName()
	name = strings.ToLower(name)
	org := strings.Split(name, "/")[0]
	subs, err := p.GetSubscriptions()
	if err != nil {
		return nil
	}

	// Add subscriptions for the specific repo
	subsForRepo := []*Subscription{}
	if subs.Repositories[name] != nil {
		subsForRepo = append(subsForRepo, subs.Repositories[name]...)
	}

	// Add subscriptions for the organization
	orgKey := fullNameFromOwnerAndRepo(org, "")
	if subs.Repositories[orgKey] != nil {
		subsForRepo = append(subsForRepo, subs.Repositories[orgKey]...)
	}

	if len(subsForRepo) == 0 {
		return nil
	}

	subsToReturn := []*Subscription{}

	for _, sub := range subsForRepo {
		if repo.GetPrivate() && !p.permissionToRepo(sub.CreatorID, name) {
			continue
		}
		if sub.excludedRepoForSub(repo) {
			continue
		}
		subsToReturn = append(subsToReturn, sub)
	}

	return subsToReturn
}

func (p *Plugin) Unsubscribe(channelID, repo, owner string) error {
	repoWithOwner := fmt.Sprintf("%s/%s", owner, repo)

	subs, err := p.GetSubscriptions()
	if err != nil {
		return errors.Wrap(err, "could not get subscriptions")
	}

	repoSubs := subs.Repositories[repoWithOwner]
	if repoSubs == nil {
		return nil
	}

	removed := false
	for index, sub := range repoSubs {
		if sub.ChannelID == channelID {
			repoSubs = append(repoSubs[:index], repoSubs[index+1:]...)
			removed = true
			break
		}
	}

	if removed {
		subs.Repositories[repoWithOwner] = repoSubs
		if err := p.StoreSubscriptions(subs); err != nil {
			return errors.Wrap(err, "could not store subscriptions")
		}
	}

	return nil
}
