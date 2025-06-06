// Copyright (c) 2018-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package plugin

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/go-github/v54/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi/experimental/command"
)

const (
	featureIssueCreation      = "issue_creations"
	featureIssues             = "issues"
	featurePulls              = "pulls"
	featurePullsMerged        = "pulls_merged"
	featurePullsCreated       = "pulls_created"
	featurePushes             = "pushes"
	featureCreates            = "creates"
	featureDeletes            = "deletes"
	featureIssueComments      = "issue_comments"
	featurePullReviews        = "pull_reviews"
	featureStars              = "stars"
	featureReleases           = "releases"
	featureWorkflowFailure    = "workflow_failure"
	featureWorkflowSuccess    = "workflow_success"
	featureDiscussions        = "discussions"
	featureDiscussionComments = "discussion_comments"
)

const (
	PerPageValue = 50
)

const DefaultRepoKey string = "%s_%s-default-repo"

var validFeatures = map[string]bool{
	featureIssueCreation:      true,
	featureIssues:             true,
	featurePulls:              true,
	featurePullsMerged:        true,
	featurePullsCreated:       true,
	featurePushes:             true,
	featureCreates:            true,
	featureDeletes:            true,
	featureIssueComments:      true,
	featurePullReviews:        true,
	featureStars:              true,
	featureReleases:           true,
	featureWorkflowFailure:    true,
	featureWorkflowSuccess:    true,
	featureDiscussions:        true,
	featureDiscussionComments: true,
}

type Features string

func (features Features) String() string {
	return string(features)
}

func (features Features) FormattedString() string {
	return "`" + strings.Join(strings.Split(features.String(), ","), "`, `") + "`"
}

func (features Features) ToSlice() []string {
	return strings.Split(string(features), ",")
}

// validateFeatures returns false when 1 or more given features
// are invalid along with a list of the invalid features.
func validateFeatures(features []string) (bool, []string) {
	valid := true
	invalidFeatures := []string{}
	hasLabel := false
	for _, f := range features {
		if _, ok := validFeatures[f]; ok {
			continue
		}
		if strings.HasPrefix(f, "label") {
			hasLabel = true
			continue
		}
		invalidFeatures = append(invalidFeatures, f)
		valid = false
	}
	if valid && hasLabel {
		// must have "pulls" or "issues" in features when using a label
		for _, f := range features {
			if f == featurePulls || f == featureIssues || f == featureIssueCreation {
				return valid, invalidFeatures
			}
		}
		valid = false
	}
	return valid, invalidFeatures
}

// checkFeatureConflict returns false when given features
// cannot be added together along with a list of the conflicting features.
func checkFeatureConflict(fs []string) (bool, []string) {
	if SliceContainsString(fs, featureIssues) && SliceContainsString(fs, featureIssueCreation) {
		return false, []string{featureIssues, featureIssueCreation}
	}
	if SliceContainsString(fs, featurePulls) && SliceContainsString(fs, featurePullsMerged) {
		return false, []string{featurePulls, featurePullsMerged}
	}
	if SliceContainsString(fs, featurePulls) && SliceContainsString(fs, featurePullsCreated) {
		return false, []string{featurePulls, featurePullsCreated}
	}
	return true, nil
}

func (p *Plugin) getCommand(config *Configuration) (*model.Command, error) {
	iconData, err := command.GetIconData(&p.client.System, "assets/icon-bg.svg")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get icon data")
	}

	return &model.Command{
		Trigger:              "github",
		AutoComplete:         true,
		AutoCompleteDesc:     "Available commands: connect, disconnect, todo, subscriptions, issue, default-repo, me, mute, settings, help, about",
		AutoCompleteHint:     "[command]",
		AutocompleteData:     getAutocompleteData(config),
		AutocompleteIconData: iconData,
	}, nil
}

func (p *Plugin) postCommandResponse(args *model.CommandArgs, text string) {
	post := &model.Post{
		UserId:    p.BotUserID,
		ChannelId: args.ChannelId,
		RootId:    args.RootId,
		Message:   text,
	}
	p.client.Post.SendEphemeralPost(args.UserId, post)
}

func (p *Plugin) getMutedUsernames(userInfo *GitHubUserInfo) ([]string, error) {
	var mutedUsernameBytes []byte
	err := p.store.Get(userInfo.UserID+"-muted-users", &mutedUsernameBytes)
	if err != nil {
		return nil, err
	}
	mutedUsernames := string(mutedUsernameBytes)
	var mutedUsers []string
	if len(mutedUsernames) == 0 {
		return mutedUsers, nil
	}
	mutedUsers = strings.Split(mutedUsernames, ",")
	return mutedUsers, nil
}

func (p *Plugin) handleMuteList(_ *model.CommandArgs, userInfo *GitHubUserInfo) string {
	mutedUsernames, err := p.getMutedUsernames(userInfo)
	if err != nil {
		p.client.Log.Error("error occurred getting muted users.", "UserID", userInfo.UserID, "Error", err)
		return "An error occurred getting muted users. Please try again later"
	}

	var mutedUsers string
	for _, user := range mutedUsernames {
		mutedUsers += fmt.Sprintf("- %v\n", user)
	}
	if len(mutedUsers) == 0 {
		return "You have no muted users"
	}
	return "Your muted users:\n" + mutedUsers
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func (p *Plugin) handleMuteAdd(_ *model.CommandArgs, username string, userInfo *GitHubUserInfo) string {
	mutedUsernames, err := p.getMutedUsernames(userInfo)
	if err != nil {
		p.client.Log.Error("error occurred getting muted users.", "UserID", userInfo.UserID, "Error", err)
		return "An error occurred getting muted users. Please try again later"
	}

	if contains(mutedUsernames, username) {
		return username + " is already muted"
	}

	if strings.Contains(username, ",") {
		return "Invalid username provided"
	}

	var mutedUsers string
	if len(mutedUsernames) > 0 {
		// , is a character not allowed in github usernames so we can split on them
		mutedUsers = strings.Join(mutedUsernames, ",") + "," + username
	} else {
		mutedUsers = username
	}

	_, err = p.store.Set(userInfo.UserID+"-muted-users", []byte(mutedUsers))
	if err != nil {
		return "Error occurred saving list of muted users"
	}

	return fmt.Sprintf("`%v`", username) + " is now muted. You'll no longer receive notifications for comments in your PRs and issues."
}

func (p *Plugin) handleUnmute(_ *model.CommandArgs, username string, userInfo *GitHubUserInfo) string {
	mutedUsernames, err := p.getMutedUsernames(userInfo)
	if err != nil {
		p.client.Log.Error("error occurred getting muted users.", "UserID", userInfo.UserID, "Error", err)
		return "An error occurred getting muted users. Please try again later"
	}

	userToMute := []string{username}
	newMutedList := arrayDifference(mutedUsernames, userToMute)

	_, err = p.store.Set(userInfo.UserID+"-muted-users", []byte(strings.Join(newMutedList, ",")))
	if err != nil {
		return "Error occurred unmuting users"
	}

	return fmt.Sprintf("`%v`", username) + " is no longer muted"
}

func (p *Plugin) handleUnmuteAll(_ *model.CommandArgs, userInfo *GitHubUserInfo) string {
	_, err := p.store.Set(userInfo.UserID+"-muted-users", []byte(""))
	if err != nil {
		return "Error occurred unmuting users"
	}

	return "Unmuted all users"
}

func (p *Plugin) handleMuteCommand(_ *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid mute command. Available commands are 'list', 'add' and 'delete'."
	}

	command := parameters[0]

	switch {
	case command == "list":
		return p.handleMuteList(args, userInfo)
	case command == "add":
		if len(parameters) != 2 {
			return "Invalid number of parameters supplied to " + command
		}
		return p.handleMuteAdd(args, parameters[1], userInfo)
	case command == "delete":
		if len(parameters) != 2 {
			return "Invalid number of parameters supplied to " + command
		}
		return p.handleUnmute(args, parameters[1], userInfo)
	case command == "delete-all":
		return p.handleUnmuteAll(args, userInfo)
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

// Returns the elements in a, that are not in b
func arrayDifference(a, b []string) []string {
	mb := make(map[string]struct{}, len(b))
	for _, x := range b {
		mb[x] = struct{}{}
	}
	var diff []string
	for _, x := range a {
		if _, found := mb[x]; !found {
			diff = append(diff, x)
		}
	}
	return diff
}

func (p *Plugin) handleSubscribe(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	switch {
	case len(parameters) == 0:
		return "Please specify a repository or 'list' command."
	case len(parameters) == 1 && parameters[0] == "list":
		return p.handleSubscriptionsList(c, args, parameters[1:], userInfo)
	default:
		return p.handleSubscribesAdd(c, args, parameters, userInfo)
	}
}

func (p *Plugin) handleSubscriptions(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid subscribe command. Available commands are 'list', 'add' and 'delete'."
	}

	command := parameters[0]
	parameters = parameters[1:]

	switch {
	case command == "list":
		return p.handleSubscriptionsList(c, args, parameters, userInfo)
	case command == "add":
		return p.handleSubscribesAdd(c, args, parameters, userInfo)
	case command == "delete":
		return p.handleUnsubscribe(c, args, parameters, userInfo)
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

func (p *Plugin) handleSubscriptionsList(_ *plugin.Context, args *model.CommandArgs, _ []string, _ *GitHubUserInfo) string {
	txt := ""
	subs, err := p.GetSubscriptionsByChannel(args.ChannelId)
	if err != nil {
		return err.Error()
	}

	if len(subs) == 0 {
		txt = "Currently there are no subscriptions in this channel"
	} else {
		txt = "### Subscriptions in this channel\n"
	}
	for _, sub := range subs {
		subFlags := sub.Flags.String()
		txt += fmt.Sprintf("* `%s` - %s", strings.Trim(sub.Repository, "/"), sub.Features.String())
		if subFlags != "" {
			txt += fmt.Sprintf(" %s", subFlags)
		}
		txt += "\n"
	}

	return txt
}

func (p *Plugin) createPost(channelID, userID, message string) error {
	post := &model.Post{
		ChannelId: channelID,
		UserId:    userID,
		Message:   message,
	}

	if err := p.client.Post.CreatePost(post); err != nil {
		p.client.Log.Warn("Error while creating post", "post", post, "error", err.Error())
		return err
	}

	return nil
}

func (p *Plugin) checkIfConfiguredWebhookExists(ctx context.Context, githubClient *github.Client, userInfo *GitHubUserInfo, repo, owner string) (bool, error) {
	found := false
	opt := &github.ListOptions{
		PerPage: PerPageValue,
	}
	siteURL, err := getSiteURL(p.client)
	if err != nil {
		return false, err
	}

	for {
		var githubHooks []*github.Hook
		var githubResponse *github.Response
		var err, cErr error

		if repo == "" {
			cErr = p.useGitHubClient(userInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
				githubHooks, githubResponse, err = githubClient.Organizations.ListHooks(ctx, owner, opt)
				if err != nil {
					return err
				}
				return nil
			})
		} else {
			cErr = p.useGitHubClient(userInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
				githubHooks, githubResponse, err = githubClient.Repositories.ListHooks(ctx, owner, repo, opt)
				if err != nil {
					return err
				}
				return nil
			})
		}

		if cErr != nil {
			p.client.Log.Warn("Not able to get the list of webhooks", "Owner", owner, "Repo", repo, "error", err.Error())
			return found, err
		}

		for _, hook := range githubHooks {
			if strings.Contains(hook.Config["url"].(string), siteURL) {
				found = true
				break
			}
		}

		if githubResponse.NextPage == 0 {
			break
		}
		opt.Page = githubResponse.NextPage
	}

	return found, nil
}

func (p *Plugin) handleSubscribesAdd(_ *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	const errorNoWebhookFound = "\n**Note:** No webhook was found for this repository or organization. To create one, enter the following slash command `/github setup webhook`"
	subscriptionEvents := Features("pulls,issues,creates,deletes")
	if len(parameters) == 0 {
		return "Please specify a repository."
	}
	config := p.getConfiguration()
	baseURL := config.getBaseURL()

	flags := SubscriptionFlags{}
	if len(parameters) > 1 {
		flagParams := parameters[1:]

		if len(flagParams)%2 != 0 {
			return "Please use the correct format for flags: --<name> <value>"
		}
		for i := 0; i < len(flagParams); i += 2 {
			flag := flagParams[i]
			value := flagParams[i+1]

			if !isFlag(flag) {
				return "Please use the correct format for flags: --<name> <value>"
			}
			parsedFlag := parseFlag(flag)

			if parsedFlag == flagFeatures {
				subscriptionEvents = Features(value)
				continue
			}
			if err := flags.AddFlag(parsedFlag, value); err != nil {
				return fmt.Sprintf("Unsupported value for flag %s", flag)
			}
		}

		fs := subscriptionEvents.ToSlice()

		ok, conflictingFs := checkFeatureConflict(fs)

		if !ok {
			if len(conflictingFs) == 2 {
				return fmt.Sprintf("Feature list cannot contain both %s and %s", conflictingFs[0], conflictingFs[1])
			}
			return fmt.Sprintf("Conflicting feature(s) provided: %s", strings.Join(conflictingFs, ","))
		}

		ok, ifs := validateFeatures(fs)
		if !ok {
			msg := fmt.Sprintf("Invalid feature(s) provided: %s", strings.Join(ifs, ","))
			if len(ifs) == 0 {
				msg = "Feature list must have \"pulls\", \"issues\" or \"issue_creations\" when using a label."
			}
			return msg
		}
	}

	ctx := context.Background()
	githubClient := p.githubConnectUser(ctx, userInfo)
	user, err := p.client.User.Get(args.UserId)
	if err != nil {
		return errors.Wrap(err, "failed to get the user").Error()
	}

	owner, repo := parseOwnerAndRepo(parameters[0], baseURL)
	previousSubscribedEvents, err := p.getSubscribedFeatures(args.ChannelId, owner, repo)
	if err != nil {
		return errors.Wrap(err, "failed to get the subscribed events").Error()
	}

	var previousSubscribedEventMessage string
	if previousSubscribedEvents != "" {
		previousSubscribedEventMessage = fmt.Sprintf("\nThe previous subscription with: %s was overwritten.\n", previousSubscribedEvents.FormattedString())
	}

	if repo == "" {
		if err = p.SubscribeOrg(ctx, githubClient, args.UserId, owner, args.ChannelId, subscriptionEvents, flags); err != nil {
			return errors.Wrap(err, "failed to get the subscribed org").Error()
		}
		orgLink := baseURL + owner
		subscriptionSuccess := fmt.Sprintf("@%v subscribed this channel to [%s](%s) with the following events: %s.", user.Username, owner, orgLink, subscriptionEvents.FormattedString())

		if previousSubscribedEvents != "" {
			subscriptionSuccess += previousSubscribedEventMessage
		}

		if err = p.createPost(args.ChannelId, p.BotUserID, subscriptionSuccess); err != nil {
			return fmt.Sprintf("%s error creating the public post: %s", subscriptionSuccess, err.Error())
		}

		subOrgMsg := fmt.Sprintf("Successfully subscribed to organization %s.", owner)

		found, foundErr := p.checkIfConfiguredWebhookExists(ctx, githubClient, userInfo, repo, owner)
		if foundErr != nil {
			if strings.Contains(foundErr.Error(), "404 Not Found") {
				// We are not returning an error here and just a subscription success message, as the above error condition occurs when the user is not authorized to access webhooks.
				return ""
			}
			return errors.Wrap(foundErr, "failed to get the list of webhooks").Error()
		}

		if !found {
			subOrgMsg = errorNoWebhookFound
		}
		return subOrgMsg
	}

	if len(flags.ExcludeRepository) > 0 {
		return "Exclude repository feature is only available to subscriptions of an organization."
	}

	if err = p.Subscribe(ctx, githubClient, args.UserId, owner, repo, args.ChannelId, subscriptionEvents, flags); err != nil {
		return errors.Wrap(err, "failed to create a subscription").Error()
	}
	repoLink := config.getBaseURL() + owner + "/" + repo

	msg := fmt.Sprintf("@%v subscribed this channel to [%s/%s](%s) with the following events: %s", user.Username, owner, repo, repoLink, subscriptionEvents.FormattedString())
	if previousSubscribedEvents != "" {
		msg += previousSubscribedEventMessage
	}

	if cErr := p.useGitHubClient(userInfo, func(userInfo *GitHubUserInfo, token *oauth2.Token) error {
		var ghRepo *github.Repository
		ghRepo, _, err = githubClient.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return err
		} else if ghRepo != nil && ghRepo.GetPrivate() {
			msg += "\n\n**Warning:** You subscribed to a private repository. Anyone with access to this channel will be able to read the events getting posted here."
		}
		return nil
	}); cErr != nil {
		p.client.Log.Warn("Failed to fetch repository", "error", cErr.Error())
	}

	if err = p.createPost(args.ChannelId, p.BotUserID, msg); err != nil {
		return fmt.Sprintf("%s\nError creating the public post: %s", msg, err.Error())
	}

	found, err := p.checkIfConfiguredWebhookExists(ctx, githubClient, userInfo, repo, owner)
	if err != nil {
		if strings.Contains(err.Error(), "404 Not Found") {
			// We are not returning an error here and just a subscription success message, as the above error condition occurs when the user is not authorized to access webhooks.
			return ""
		}
		return errors.Wrap(err, "failed to get the list of webhooks").Error()
	}

	if !found {
		msg = errorNoWebhookFound
	}

	return msg
}

func (p *Plugin) getSubscribedFeatures(channelID, owner, repo string) (Features, error) {
	var previousFeatures Features
	subs, err := p.GetSubscriptionsByChannel(channelID)
	if err != nil {
		return previousFeatures, err
	}

	for _, sub := range subs {
		fullRepoName := repo
		if owner != "" {
			fullRepoName = owner + "/" + repo
		}

		if sub.Repository == fullRepoName {
			previousFeatures = sub.Features
			return previousFeatures, nil
		}
	}

	return previousFeatures, nil
}

func (p *Plugin) handleUnsubscribe(_ *plugin.Context, args *model.CommandArgs, parameters []string, _ *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Please specify a repository."
	}

	repo := parameters[0]
	config := p.getConfiguration()
	owner, repo := parseOwnerAndRepo(repo, config.getBaseURL())
	if owner == "" && repo == "" {
		return "invalid repository"
	}

	owner = strings.ToLower(owner)
	repo = strings.ToLower(repo)
	if err := p.Unsubscribe(args.ChannelId, repo, owner); err != nil {
		p.client.Log.Warn("Failed to unsubscribe", "repo", repo, "error", err.Error())
		return "Encountered an error trying to unsubscribe. Please try again."
	}

	baseURL := config.getBaseURL()
	user, err := p.client.User.Get(args.UserId)
	if err != nil {
		p.client.Log.Warn("Error while fetching user details", "error", err.Error())
		return fmt.Sprintf("error while fetching user details: %s", err.Error())
	}

	unsubscribeMessage := ""
	if repo == "" {
		orgLink := baseURL + owner
		unsubscribeMessage = fmt.Sprintf("@%v unsubscribed this channel from [%s](%s)", user.Username, owner, orgLink)

		if err := p.createPost(args.ChannelId, p.BotUserID, unsubscribeMessage); err != nil {
			return fmt.Sprintf("%s error creating the public post: %s", unsubscribeMessage, err.Error())
		}

		return ""
	}

	repoLink := baseURL + owner + "/" + repo
	unsubscribeMessage = fmt.Sprintf("@%v Unsubscribed this channel from [%s/%s](%s)", user.Username, owner, repo, repoLink)
	unsubscribeMessage += fmt.Sprintf("\n Please delete the [webhook](%s) for this subscription unless it's required for other subscriptions.", fmt.Sprintf("%s/settings/hooks", repoLink))

	if err := p.createPost(args.ChannelId, p.BotUserID, unsubscribeMessage); err != nil {
		return fmt.Sprintf("%s error creating the public post: %s", unsubscribeMessage, err.Error())
	}

	return ""
}

func (p *Plugin) handleDisconnect(_ *plugin.Context, args *model.CommandArgs, _ []string, _ *GitHubUserInfo) string {
	p.disconnectGitHubAccount(args.UserId)
	return "Disconnected your GitHub account."
}

func (p *Plugin) handleTodo(_ *plugin.Context, _ *model.CommandArgs, _ []string, userInfo *GitHubUserInfo) string {
	githubClient := p.githubConnectUser(context.Background(), userInfo)

	text, err := p.GetToDo(context.Background(), userInfo, githubClient)
	if err != nil {
		p.client.Log.Warn("Failed get get Todos", "error", err.Error())
		return "Encountered an error getting your to do items."
	}

	return text
}

func (p *Plugin) handleMe(_ *plugin.Context, _ *model.CommandArgs, _ []string, userInfo *GitHubUserInfo) string {
	githubClient := p.githubConnectUser(context.Background(), userInfo)
	var gitUser *github.User
	cErr := p.useGitHubClient(userInfo, func(userInfo *GitHubUserInfo, token *oauth2.Token) error {
		resp, _, err := githubClient.Users.Get(context.Background(), "")
		if err != nil {
			return err
		}
		gitUser = resp
		return nil
	})
	if cErr != nil {
		return "Encountered an error getting your GitHub profile."
	}

	text := fmt.Sprintf("You are connected to GitHub as:\n# [![image](%s =40x40)](%s) [%s](%s)", gitUser.GetAvatarURL(), gitUser.GetHTMLURL(), gitUser.GetLogin(), gitUser.GetHTMLURL())
	return text
}

func (p *Plugin) handleHelp(_ *plugin.Context, _ *model.CommandArgs, _ []string, _ *GitHubUserInfo) string {
	message, err := renderTemplate("helpText", p.getConfiguration())
	if err != nil {
		p.client.Log.Warn("Failed to render help template", "error", err.Error())
		return "Encountered an error posting help text."
	}

	return "###### Mattermost GitHub Plugin - Slash Command Help\n" + message
}

func (p *Plugin) handleSettings(_ *plugin.Context, _ *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) < 2 {
		return "Please specify both a setting and value. Use `/github help` for more usage information."
	}

	setting := parameters[0]
	settingValue := parameters[1]

	switch setting {
	case settingNotifications:
		switch settingValue {
		case settingOn:
			userInfo.Settings.Notifications = true
		case settingOff:
			userInfo.Settings.Notifications = false
		default:
			return "Invalid value. Accepted values are: \"on\" or \"off\"."
		}
	case settingReminders:
		switch settingValue {
		case settingOn:
			userInfo.Settings.DailyReminder = true
			userInfo.Settings.DailyReminderOnChange = false
		case settingOff:
			userInfo.Settings.DailyReminder = false
			userInfo.Settings.DailyReminderOnChange = false
		case settingOnChange:
			userInfo.Settings.DailyReminder = true
			userInfo.Settings.DailyReminderOnChange = true
		default:
			return "Invalid value. Accepted values are: \"on\" or \"off\" or \"on-change\" ."
		}
	default:
		return "Unknown setting " + setting
	}

	if setting == settingNotifications {
		if userInfo.Settings.Notifications {
			err := p.storeGitHubToUserIDMapping(userInfo.GitHubUsername, userInfo.UserID)
			if err != nil {
				p.client.Log.Warn("Failed to store GitHub to userID mapping",
					"userID", userInfo.UserID,
					"GitHub username", userInfo.GitHubUsername,
					"error", err.Error())
			}
		} else {
			err := p.store.Delete(userInfo.GitHubUsername + githubUsernameKey)
			if err != nil {
				p.client.Log.Warn("Failed to delete GitHub to userID mapping",
					"userID", userInfo.UserID,
					"GitHub username", userInfo.GitHubUsername,
					"error", err.Error())
			}
		}
	}

	err := p.storeGitHubUserInfo(userInfo)
	if err != nil {
		p.client.Log.Warn("Failed to store github user info", "error", err.Error())
		return "Failed to store settings"
	}

	return "Settings updated."
}

func (p *Plugin) handleIssue(_ *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid issue command. Available command is 'create'."
	}

	command := parameters[0]
	parameters = parameters[1:]

	switch {
	case command == "create":
		p.openIssueCreateModal(args.UserId, args.ChannelId, strings.Join(parameters, " "))
		return ""
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

func (p *Plugin) handleDefaultRepo(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid action. Available actions are 'set', 'get' and 'unset'."
	}

	command := parameters[0]
	parameters = parameters[1:]

	switch {
	case command == "set":
		return p.handleSetDefaultRepo(args, parameters, userInfo)
	case command == "get":
		return p.handleGetDefaultRepo(args, userInfo)
	case command == "unset":
		return p.handleUnSetDefaultRepo(args, userInfo)
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

func (p *Plugin) handleSetDefaultRepo(args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Please specify a repository."
	}

	repo := parameters[0]
	config := p.getConfiguration()
	baseURL := config.getBaseURL()
	owner, repo := parseOwnerAndRepo(repo, baseURL)
	if owner == "" || repo == "" {
		return "Please provide a valid repository"
	}

	owner = strings.ToLower(owner)
	repo = strings.ToLower(repo)

	if config.GitHubOrg != "" && strings.ToLower(config.GitHubOrg) != owner {
		return fmt.Sprintf("Repository is not part of the locked Github organization. Locked Github organization: %s", config.GitHubOrg)
	}

	ctx := context.Background()
	githubClient := p.githubConnectUser(ctx, userInfo)

	ghRepo, _, err := githubClient.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "Error occurred while getting github repository details"
	}
	if ghRepo == nil {
		return fmt.Sprintf("Unknown repository %s", fullNameFromOwnerAndRepo(owner, repo))
	}

	if _, err := p.store.Set(fmt.Sprintf(DefaultRepoKey, args.ChannelId, userInfo.UserID), []byte(fmt.Sprintf("%s/%s", owner, repo))); err != nil {
		return "Error occurred saving the default repo"
	}

	repoLink := fmt.Sprintf("%s%s/%s", baseURL, owner, repo)
	successMsg := fmt.Sprintf("The default repo has been set to [%s/%s](%s) for this channel", owner, repo, repoLink)

	return successMsg
}

func (p *Plugin) GetDefaultRepo(userID, channelID string) (string, error) {
	var defaultRepoBytes []byte
	if err := p.store.Get(fmt.Sprintf(DefaultRepoKey, channelID, userID), &defaultRepoBytes); err != nil {
		return "", err
	}

	return string(defaultRepoBytes), nil
}

func (p *Plugin) handleGetDefaultRepo(args *model.CommandArgs, userInfo *GitHubUserInfo) string {
	defaultRepo, err := p.GetDefaultRepo(userInfo.UserID, args.ChannelId)
	if err != nil {
		p.client.Log.Warn("Not able to get the default repo", "UserID", userInfo.UserID, "ChannelID", args.ChannelId, "Error", err.Error())
		return "Error occurred while getting the default repo"
	}

	if defaultRepo == "" {
		return "You have not set a default repository for this channel"
	}

	config := p.getConfiguration()
	repoLink := config.getBaseURL() + defaultRepo
	return fmt.Sprintf("The default repository is [%s](%s)", defaultRepo, repoLink)
}

func (p *Plugin) handleUnSetDefaultRepo(args *model.CommandArgs, userInfo *GitHubUserInfo) string {
	defaultRepo, err := p.GetDefaultRepo(userInfo.UserID, args.ChannelId)
	if err != nil {
		p.client.Log.Warn("Not able to get the default repo", "UserID", userInfo.UserID, "ChannelID", args.ChannelId, "Error", err.Error())
		return "Error occurred while getting the default repo"
	}

	if defaultRepo == "" {
		return "You have not set a default repository for this channel"
	}

	if err := p.store.Delete(fmt.Sprintf(DefaultRepoKey, args.ChannelId, userInfo.UserID)); err != nil {
		return "Error occurred while unsetting the repo for this channel"
	}

	return "The default repository has been unset successfully"
}

func (p *Plugin) handleSetup(_ *plugin.Context, args *model.CommandArgs, parameters []string) string {
	userID := args.UserId
	isSysAdmin, err := p.isAuthorizedSysAdmin(userID)
	if err != nil {
		p.client.Log.Warn("Failed to check if user is System Admin", "error", err.Error())

		return "Error checking user's permissions"
	}

	if !isSysAdmin {
		return "Only System Admins are allowed to set up the plugin."
	}

	if len(parameters) == 0 {
		err = p.flowManager.StartSetupWizard(userID, "")
	} else {
		command := parameters[0]

		switch {
		case command == "oauth":
			err = p.flowManager.StartOauthWizard(userID)
		case command == "webhook":
			err = p.flowManager.StartWebhookWizard(userID)
		case command == "announcement":
			err = p.flowManager.StartAnnouncementWizard(userID)
		default:
			return fmt.Sprintf("Unknown subcommand %v", command)
		}
	}

	if err != nil {
		return err.Error()
	}

	return ""
}

type CommandHandleFunc func(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string

func (p *Plugin) isAuthorizedSysAdmin(userID string) (bool, error) {
	user, err := p.client.User.Get(userID)
	if err != nil {
		return false, err
	}
	if !strings.Contains(user.Roles, "system_admin") {
		return false, nil
	}
	return true, nil
}

func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	cmd, action, parameters := parseCommand(args.Command)

	if cmd != "/github" {
		return &model.CommandResponse{}, nil
	}

	if action == "about" {
		text, err := command.BuildInfo(model.Manifest{
			Id:      Manifest.Id,
			Version: Manifest.Version,
			Name:    Manifest.Name,
		})
		if err != nil {
			text = errors.Wrap(err, "failed to get build info").Error()
		}
		p.postCommandResponse(args, text)
		return &model.CommandResponse{}, nil
	}

	if action == "setup" {
		message := p.handleSetup(c, args, parameters)
		if message != "" {
			p.postCommandResponse(args, message)
		}
		return &model.CommandResponse{}, nil
	}

	config := p.getConfiguration()

	if validationErr := config.IsValid(); validationErr != nil {
		isSysAdmin, err := p.isAuthorizedSysAdmin(args.UserId)
		var text string
		switch {
		case err != nil:
			text = "Error checking user's permissions"
			p.client.Log.Warn(text, "error", err.Error())
		case isSysAdmin:
			text = fmt.Sprintf("Before using this plugin, you'll need to configure it by running `/github setup`: %s", validationErr.Error())
		default:
			text = "Please contact your system administrator to correctly configure the GitHub plugin."
		}

		p.postCommandResponse(args, text)
		return &model.CommandResponse{}, nil
	}

	if action == "connect" {
		connectURL, err := buildPluginURL(p.client, "oauth", "connect")
		if err != nil {
			p.postCommandResponse(args, fmt.Sprintf("Encountered an error connecting to GitHub: %s", err.Error()))
			return &model.CommandResponse{}, nil
		}

		privateAllowed := p.getConfiguration().ConnectToPrivateByDefault
		if len(parameters) > 0 {
			if privateAllowed {
				p.postCommandResponse(args, fmt.Sprintf("Unknown command `%v`. Do you meant `/github connect`?", args.Command))
				return &model.CommandResponse{}, nil
			}

			if len(parameters) != 1 || parameters[0] != "private" {
				p.postCommandResponse(args, fmt.Sprintf("Unknown command `%v`. Do you meant `/github connect private`?", args.Command))
				return &model.CommandResponse{}, nil
			}

			privateAllowed = true
		}

		qparams := ""
		if privateAllowed {
			if !p.getConfiguration().EnablePrivateRepo {
				p.postCommandResponse(args, "Private repositories are disabled. Please ask a System Admin to enabled them.")
				return &model.CommandResponse{}, nil
			}
			qparams = "?private=true"
		}

		msg := fmt.Sprintf("[Click here to link your GitHub account.](%s%s)", connectURL, qparams)
		p.postCommandResponse(args, msg)
		return &model.CommandResponse{}, nil
	}

	info, apiErr := p.getGitHubUserInfo(args.UserId)
	if apiErr != nil {
		text := "Unknown error."
		if apiErr.ID == apiErrorIDNotConnected {
			text = "You must connect your account to GitHub first. Either click on the GitHub logo in the bottom left of the screen or enter `/github connect`."
		}
		p.postCommandResponse(args, text)
		return &model.CommandResponse{}, nil
	}

	if f, ok := p.CommandHandlers[action]; ok {
		message := f(c, args, parameters, info)
		if message != "" {
			p.postCommandResponse(args, message)
		}
		return &model.CommandResponse{}, nil
	}

	p.postCommandResponse(args, fmt.Sprintf("Unknown action %v", action))
	return &model.CommandResponse{}, nil
}

func getAutocompleteData(config *Configuration) *model.AutocompleteData {
	if !config.IsOAuthConfigured() {
		github := model.NewAutocompleteData("github", "[command]", "Available commands: setup, about")

		setup := model.NewAutocompleteData("setup", "", "Set up the GitHub plugin")
		setup.RoleID = model.SystemAdminRoleId
		github.AddCommand(setup)

		about := command.BuildInfoAutocomplete("about")
		github.AddCommand(about)

		return github
	}

	github := model.NewAutocompleteData("github", "[command]", "Available commands: connect, disconnect, todo, subscriptions, issue, default-repo, me, mute, settings, help, about")

	connect := model.NewAutocompleteData("connect", "", "Connect your Mattermost account to your GitHub account")
	if config.EnablePrivateRepo {
		if config.ConnectToPrivateByDefault {
			connect = model.NewAutocompleteData("connect", "", "Connect your Mattermost account to your GitHub account. Read access to your private repositories will be requested")
		} else {
			private := model.NewAutocompleteData("private", "(optional)", "If used, read access to your private repositories will be requested")
			connect.AddCommand(private)
		}
	}
	github.AddCommand(connect)

	disconnect := model.NewAutocompleteData("disconnect", "", "Disconnect your Mattermost account from your GitHub account")
	github.AddCommand(disconnect)

	todo := model.NewAutocompleteData("todo", "", "Get a list of unread messages and pull requests awaiting your review")
	github.AddCommand(todo)

	subscriptions := model.NewAutocompleteData("subscriptions", "[command]", "Available commands: list, add, delete")

	subscribeList := model.NewAutocompleteData("list", "", "List the current channel subscriptions")
	subscriptions.AddCommand(subscribeList)

	subscriptionsAdd := model.NewAutocompleteData("add", "[owner/repo] [features] [flags]", "Subscribe the current channel to receive notifications about opened pull requests and issues for an organization or repository. [features] and [flags] are optional arguments")
	subscriptionsAdd.AddTextArgument("Owner/repo to subscribe to", "[owner/repo]", "")
	subscriptionsAdd.AddNamedTextArgument("features", "Comma-delimited list of one or more of: issues, pulls, pulls_merged, pulls_created, pushes, creates, deletes, issue_creations, issue_comments, pull_reviews, releases, workflow_success, workflow_failure, discussions, discussion_comments, label:\"<labelname>\". Defaults to pulls,issues,creates,deletes", "", `/[^,-\s]+(,[^,-\s]+)*/`, false)

	if config.GitHubOrg != "" {
		subscriptionsAdd.AddNamedStaticListArgument("exclude-org-member", "Events triggered by organization members will not be delivered (the organization config should be set, otherwise this flag has not effect)", false, []model.AutocompleteListItem{
			{
				Item:     "true",
				HelpText: "Exclude posts from members of the configured organization",
			},
			{
				Item:     "false",
				HelpText: "Include posts from members of the configured organization",
			},
		})
		subscriptionsAdd.AddNamedStaticListArgument("include-only-org-members", "Events triggered only by organization members will be delivered (the organization config should be set, otherwise this flag has not effect)", false, []model.AutocompleteListItem{
			{
				Item:     "true",
				HelpText: "Include posts only from members of the configured organization",
			},
			{
				Item:     "false",
				HelpText: "Include posts from members and collaborators of the configured organization",
			},
		})
	}

	subscriptionsAdd.AddNamedStaticListArgument("render-style", "Determine the rendering style of various notifications.", false, []model.AutocompleteListItem{
		{
			Item:     "default",
			HelpText: "The default rendering style for all notifications (includes all information).",
		},
		{
			Item:     "skip-body",
			HelpText: "Skips the body part of various long notifications that have a body (e.g. new PRs and new issues).",
		},
		{
			Item:     "collapsed",
			HelpText: "Notifications come in a one-line format, without enlarged fonts or advanced layouts.",
		},
	})

	subscriptionsAdd.AddNamedTextArgument("exclude", "Comma separated list of the repositories to exclude getting the notifications. Only supported for subscriptions to an organization", "", `/[^,-\s]+(,[^,-\s]+)*/`, false)

	subscriptions.AddCommand(subscriptionsAdd)
	subscriptionsDelete := model.NewAutocompleteData("delete", "[owner/repo]", "Unsubscribe the current channel from an organization or repository")
	subscriptionsDelete.AddTextArgument("Owner/repo to unsubscribe from", "[owner/repo]", "")
	subscriptions.AddCommand(subscriptionsDelete)

	github.AddCommand(subscriptions)

	issue := model.NewAutocompleteData("issue", "[command]", "Available commands: create")

	issueCreate := model.NewAutocompleteData("create", "[title]", "Open a dialog to create a new issue in GitHub, using the title if provided")
	issueCreate.AddTextArgument("Title for the GitHub issue", "[title]", "")
	issue.AddCommand(issueCreate)

	github.AddCommand(issue)

	defaultRepo := model.NewAutocompleteData("default-repo", "[command]", "Available commands: set, get, unset")
	defaultRepoSet := model.NewAutocompleteData("set", "[owner/repo]", "Set the default repository for the channel")
	defaultRepoSet.AddTextArgument("Owner/repo to set as a default repository", "[owner/repo]", "")

	defaultRepoGet := model.NewAutocompleteData("get", "", "Get the default repository already set for the channel")

	defaultRepoDelete := model.NewAutocompleteData("unset", "", "Unset the default repository set for the channel")

	defaultRepo.AddCommand(defaultRepoSet)
	defaultRepo.AddCommand(defaultRepoGet)
	defaultRepo.AddCommand(defaultRepoDelete)

	github.AddCommand(defaultRepo)

	me := model.NewAutocompleteData("me", "", "Display the connected GitHub account")
	github.AddCommand(me)

	mute := model.NewAutocompleteData("mute", "[command]", "Available commands: list, add, delete, delete-all")

	muteAdd := model.NewAutocompleteData("add", "[github username]", "Mute notifications from the provided GitHub user")
	muteAdd.AddTextArgument("GitHub user to mute", "[username]", "")
	mute.AddCommand(muteAdd)

	muteDelete := model.NewAutocompleteData("delete", "[github username]", "Unmute notifications from the provided GitHub user")
	muteDelete.AddTextArgument("GitHub user to unmute", "[username]", "")
	mute.AddCommand(muteDelete)

	github.AddCommand(mute)

	muteDeleteAll := model.NewAutocompleteData("delete-all", "", "Unmute all muted GitHub users")
	mute.AddCommand(muteDeleteAll)

	muteList := model.NewAutocompleteData("list", "", "List muted GitHub users")
	mute.AddCommand(muteList)

	settings := model.NewAutocompleteData("settings", "[setting] [value]", "Update your user settings")

	settingNotifications := model.NewAutocompleteData("notifications", "", "Turn notifications on/off")
	settingValue := []model.AutocompleteListItem{{
		HelpText: "Turn notifications on",
		Item:     "on",
	}, {
		HelpText: "Turn notifications off",
		Item:     "off",
	}}
	settingNotifications.AddStaticListArgument("", true, settingValue)
	settings.AddCommand(settingNotifications)

	remainderNotifications := model.NewAutocompleteData("reminders", "", "Turn notifications on/off")
	settingValue = []model.AutocompleteListItem{{
		HelpText: "Turn reminders on",
		Item:     "on",
	}, {
		HelpText: "Turn reminders off",
		Item:     "off",
	}, {
		HelpText: "Turn reminders on, but only get reminders if any changes have occurred since the previous day's reminder",
		Item:     settingOnChange,
	}}
	remainderNotifications.AddStaticListArgument("", true, settingValue)
	settings.AddCommand(remainderNotifications)

	github.AddCommand(settings)

	setup := model.NewAutocompleteData("setup", "[command]", "Available commands: oauth, webhook, announcement")
	setup.RoleID = model.SystemAdminRoleId
	setup.AddCommand(model.NewAutocompleteData("oauth", "", "Set up the OAuth2 Application in GitHub"))
	setup.AddCommand(model.NewAutocompleteData("webhook", "", "Create a webhook from GitHub to Mattermost"))
	setup.AddCommand(model.NewAutocompleteData("announcement", "", "Announce to your team that they can use GitHub integration"))
	github.AddCommand(setup)

	help := model.NewAutocompleteData("help", "", "Display Slash Command help text")
	github.AddCommand(help)

	about := command.BuildInfoAutocomplete("about")
	github.AddCommand(about)

	return github
}

// parseCommand parses the entire command input string and retrieves the command, action and parameters
func parseCommand(input string) (command, action string, parameters []string) {
	split := make([]string, 0)
	current := ""
	inQuotes := false

	for _, char := range input {
		if unicode.IsSpace(char) {
			// keep whitespaces that are inside double qoutes
			if inQuotes {
				current += " "
				continue
			}

			// ignore successive whitespaces that are outside of double quotes
			if len(current) == 0 && !inQuotes {
				continue
			}

			// append the current word to the list & move on to the next word/expression
			split = append(split, current)
			current = ""
			continue
		}

		// append the current character to the current word
		current += string(char)

		if char == '"' {
			inQuotes = !inQuotes
		}
	}

	// append the last word/expression to the list
	if len(current) > 0 {
		split = append(split, current)
	}

	command = split[0]

	if len(split) > 1 {
		action = split[1]
	}

	if len(split) > 2 {
		parameters = split[2:]
	}

	return command, action, parameters
}

func SliceContainsString(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}
