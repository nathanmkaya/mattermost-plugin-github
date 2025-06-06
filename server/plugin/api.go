// Copyright (c) 2018-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v54/github"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/mattermost/mattermost/server/public/pluginapi/experimental/bot/logger"
	"github.com/mattermost/mattermost/server/public/pluginapi/experimental/flow"

	"github.com/mattermost/mattermost-plugin-github/server/plugin/graphql"
)

const (
	apiErrorIDNotConnected = "not_connected"
	// TokenTTL is the OAuth token expiry duration in seconds
	TokenTTL = 10 * 60

	requestTimeout       = 30 * time.Second
	oauthCompleteTimeout = 2 * time.Minute

	channelIDParam = "channelId"
)

type OAuthState struct {
	UserID         string `json:"user_id"`
	Token          string `json:"token"`
	PrivateAllowed bool   `json:"private_allowed"`
}

type APIErrorResponse struct {
	ID         string `json:"id"`
	Message    string `json:"message"`
	StatusCode int    `json:"status_code"`
}

func (e *APIErrorResponse) Error() string {
	return e.Message
}

type RepoResponse struct {
	Name        string          `json:"name,omitempty"`
	FullName    string          `json:"full_name,omitempty"`
	Permissions map[string]bool `json:"permissions,omitempty"`
}

// Only send down fields to client that are needed
type RepositoryResponse struct {
	DefaultRepo RepoResponse   `json:"defaultRepo,omitempty"`
	Repos       []RepoResponse `json:"repos,omitempty"`
}

type PRDetails struct {
	URL                string                      `json:"url"`
	Number             int                         `json:"number"`
	Status             string                      `json:"status"`
	Mergeable          bool                        `json:"mergeable"`
	RequestedReviewers []*string                   `json:"requestedReviewers"`
	Reviews            []*github.PullRequestReview `json:"reviews"`
}

type FilteredNotification struct {
	github.Notification
	HTMLURL string `json:"html_url"`
}

type SidebarContent struct {
	PRs         []*graphql.GithubPRDetails `json:"prs"`
	Reviews     []*graphql.GithubPRDetails `json:"reviews"`
	Assignments []*github.Issue            `json:"assignments"`
	Unreads     []*FilteredNotification    `json:"unreads"`
}

type Context struct {
	Ctx    context.Context
	UserID string
	Log    logger.Logger
}

// HTTPHandlerFuncWithContext is http.HandleFunc but with a Context attached
type HTTPHandlerFuncWithContext func(c *Context, w http.ResponseWriter, r *http.Request)

type UserContext struct {
	Context
	GHInfo *GitHubUserInfo
}

// HTTPHandlerFuncWithUserContext is http.HandleFunc but with a UserContext attached
type HTTPHandlerFuncWithUserContext func(c *UserContext, w http.ResponseWriter, r *http.Request)

// ResponseType indicates type of response returned by api
type ResponseType string

const (
	// ResponseTypeJSON indicates that response type is json
	ResponseTypeJSON ResponseType = "JSON_RESPONSE"
	// ResponseTypePlain indicates that response type is text plain
	ResponseTypePlain ResponseType = "TEXT_RESPONSE"
)

func (p *Plugin) writeJSON(w http.ResponseWriter, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		p.client.Log.Error("Failed to marshal JSON response", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, err = w.Write(b)
	if err != nil {
		p.client.Log.Error("Failed to write JSON response", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (p *Plugin) writeAPIError(w http.ResponseWriter, apiErr *APIErrorResponse) {
	b, err := json.Marshal(apiErr)
	if err != nil {
		p.client.Log.Error("Failed to marshal API error", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(apiErr.StatusCode)

	_, err = w.Write(b)
	if err != nil {
		p.client.Log.Error("Failed to write JSON response", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (p *Plugin) initializeAPI() {
	p.router = mux.NewRouter()
	p.router.Use(p.withRecovery)

	oauthRouter := p.router.PathPrefix("/oauth").Subrouter()
	apiRouter := p.router.PathPrefix("/api/v1").Subrouter()
	apiRouter.Use(p.checkConfigured)

	p.router.HandleFunc("/webhook", p.handleWebhook).Methods(http.MethodPost)

	oauthRouter.HandleFunc("/connect", p.checkAuth(p.attachContext(p.connectUserToGitHub), ResponseTypePlain)).Methods(http.MethodGet)
	oauthRouter.HandleFunc("/complete", p.checkAuth(p.attachContext(p.completeConnectUserToGitHub), ResponseTypePlain)).Methods(http.MethodGet)

	apiRouter.HandleFunc("/connected", p.attachContext(p.getConnected)).Methods(http.MethodGet)

	apiRouter.HandleFunc("/user", p.checkAuth(p.attachContext(p.getGitHubUser), ResponseTypeJSON)).Methods(http.MethodPost)
	apiRouter.HandleFunc("/todo", p.checkAuth(p.attachUserContext(p.postToDo), ResponseTypeJSON)).Methods(http.MethodPost)
	apiRouter.HandleFunc("/prsdetails", p.checkAuth(p.attachUserContext(p.getPrsDetails), ResponseTypePlain)).Methods(http.MethodPost)
	apiRouter.HandleFunc("/searchissues", p.checkAuth(p.attachUserContext(p.searchIssues), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/createissue", p.checkAuth(p.attachUserContext(p.createIssue), ResponseTypePlain)).Methods(http.MethodPost)
	apiRouter.HandleFunc("/createissuecomment", p.checkAuth(p.attachUserContext(p.createIssueComment), ResponseTypePlain)).Methods(http.MethodPost)
	apiRouter.HandleFunc("/mentions", p.checkAuth(p.attachUserContext(p.getMentions), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/labels", p.checkAuth(p.attachUserContext(p.getLabels), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/milestones", p.checkAuth(p.attachUserContext(p.getMilestones), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/assignees", p.checkAuth(p.attachUserContext(p.getAssignees), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/organizations", p.checkAuth(p.attachUserContext(p.getOrganizations), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/repos_by_org", p.checkAuth(p.attachUserContext(p.getReposByOrg), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/repositories", p.checkAuth(p.attachUserContext(p.getRepositories), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/settings", p.checkAuth(p.attachUserContext(p.updateSettings), ResponseTypePlain)).Methods(http.MethodPost)
	apiRouter.HandleFunc("/issue", p.checkAuth(p.attachUserContext(p.getIssueByNumber), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/pr", p.checkAuth(p.attachUserContext(p.getPrByNumber), ResponseTypePlain)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/lhs-content", p.checkAuth(p.attachUserContext(p.getSidebarContent), ResponseTypePlain)).Methods(http.MethodGet)

	apiRouter.HandleFunc("/config", checkPluginRequest(p.getConfig)).Methods(http.MethodGet)
	apiRouter.HandleFunc("/token", checkPluginRequest(p.getToken)).Methods(http.MethodGet)
}

func (p *Plugin) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if x := recover(); x != nil {
				p.client.Log.Warn("Recovered from a panic",
					"url", r.URL.String(),
					"error", x,
					"stack", string(debug.Stack()))
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) checkConfigured(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := p.getConfiguration()

		if err := config.IsValid(); err != nil {
			p.client.Log.Error("This plugin is not configured.", "error", err)
			p.writeAPIError(w, &APIErrorResponse{Message: "this plugin is not configured", StatusCode: http.StatusNotImplemented})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) checkAuth(handler http.HandlerFunc, responseType ResponseType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("Mattermost-User-ID")
		if userID == "" {
			switch responseType {
			case ResponseTypeJSON:
				p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Not authorized.", StatusCode: http.StatusUnauthorized})
			case ResponseTypePlain:
				http.Error(w, "Not authorized", http.StatusUnauthorized)
			default:
				p.client.Log.Debug("Unknown ResponseType detected")
			}
			return
		}

		handler(w, r)
	}
}

func (p *Plugin) createContext(_ http.ResponseWriter, r *http.Request) (*Context, context.CancelFunc) {
	userID := r.Header.Get("Mattermost-User-ID")

	logger := logger.New(p.API).With(logger.LogContext{
		"userid": userID,
	})

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)

	context := &Context{
		Ctx:    ctx,
		UserID: userID,
		Log:    logger,
	}

	return context, cancel
}

func (p *Plugin) attachContext(handler HTTPHandlerFuncWithContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		context, cancel := p.createContext(w, r)
		defer cancel()

		handler(context, w, r)
	}
}

func (p *Plugin) attachUserContext(handler HTTPHandlerFuncWithUserContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		context, cancel := p.createContext(w, r)
		defer cancel()

		info, apiErr := p.getGitHubUserInfo(context.UserID)
		if apiErr != nil {
			p.writeAPIError(w, apiErr)
			return
		}

		context.Log = context.Log.With(logger.LogContext{
			"github username": info.GitHubUsername,
		})

		userContext := &UserContext{
			Context: *context,
			GHInfo:  info,
		}

		handler(userContext, w, r)
	}
}

func checkPluginRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// All other plugins are allowed
		pluginID := r.Header.Get("Mattermost-Plugin-ID")
		if pluginID == "" {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	p.router.ServeHTTP(w, r)
}

func (p *Plugin) connectUserToGitHub(c *Context, w http.ResponseWriter, r *http.Request) {
	privateAllowed := false
	pValBool, _ := strconv.ParseBool(r.URL.Query().Get("private"))
	if pValBool {
		privateAllowed = true
	}

	conf, err := p.getOAuthConfig(privateAllowed)
	if err != nil {
		c.Log.WithError(err).Warnf("Failed to generate OAuthConfig")
		http.Error(w, "error generating OAuthConfig", http.StatusBadRequest)
		return
	}

	state := OAuthState{
		UserID:         c.UserID,
		Token:          model.NewId()[:15],
		PrivateAllowed: privateAllowed,
	}

	_, err = p.store.Set(githubOauthKey+state.Token, state, pluginapi.SetExpiry(TokenTTL))
	if err != nil {
		c.Log.WithError(err).Errorf("error occurred while trying to store oauth state into KV store")
		p.writeAPIError(w, &APIErrorResponse{Message: "error saving the oauth state", StatusCode: http.StatusInternalServerError})
		return
	}

	url := conf.AuthCodeURL(state.Token, oauth2.AccessTypeOffline)

	ch := p.oauthBroker.SubscribeOAuthComplete(c.UserID)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		var errorMsg string
		select {
		case err := <-ch:
			if err != nil {
				errorMsg = err.Error()
			}
		case <-ctx.Done():
			errorMsg = "Timed out waiting for OAuth connection. Please check if the SiteURL is correct."
		}

		if errorMsg != "" {
			_, err := p.poster.DMWithAttachments(c.UserID, &model.SlackAttachment{
				Text:  fmt.Sprintf("There was an error connecting to your GitHub: `%s` Please double check your configuration.", errorMsg),
				Color: string(flow.ColorDanger),
			})
			if err != nil {
				c.Log.WithError(err).Warnf("Failed to DM with cancel information")
			}
		}

		p.oauthBroker.UnsubscribeOAuthComplete(c.UserID, ch)
	}()

	http.Redirect(w, r, url, http.StatusFound)
}

func (p *Plugin) completeConnectUserToGitHub(c *Context, w http.ResponseWriter, r *http.Request) {
	var rErr error
	defer func() {
		p.oauthBroker.publishOAuthComplete(c.UserID, rErr, false)
	}()

	code := r.URL.Query().Get("code")
	if len(code) == 0 {
		p.client.Log.Error("Missing authorization code.")
		p.writeAPIError(w, &APIErrorResponse{Message: "missing authorization code", StatusCode: http.StatusBadRequest})
		return
	}

	stateToken := r.URL.Query().Get("state")

	var state OAuthState
	err := p.store.Get(githubOauthKey+stateToken, &state)
	if err != nil {
		c.Log.WithError(err).Warnf("error occurred while trying to get oauth state from KV store")
		p.writeAPIError(w, &APIErrorResponse{Message: "missing stored state", StatusCode: http.StatusBadRequest})
		return
	}

	err = p.store.Delete(githubOauthKey + stateToken)
	if err != nil {
		c.Log.WithError(err).Errorf("error occurred while trying to delete oauth state from KV store")
		p.writeAPIError(w, &APIErrorResponse{Message: "error deleting stored state", StatusCode: http.StatusInternalServerError})
		return
	}

	if state.Token != stateToken {
		p.writeAPIError(w, &APIErrorResponse{Message: "invalid state token", StatusCode: http.StatusBadRequest})
		return
	}

	if state.UserID != c.UserID {
		c.Log.Warnf("not authorized, incorrect user")
		p.writeAPIError(w, &APIErrorResponse{Message: "unauthorized user", StatusCode: http.StatusUnauthorized})
		return
	}

	conf, err := p.getOAuthConfig(state.PrivateAllowed)
	if err != nil {
		c.Log.WithError(err).Warnf("Failed to generate OAuthConfig")
		http.Error(w, "error generating OAuthConfig", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), oauthCompleteTimeout)
	defer cancel()

	tok, err := conf.Exchange(ctx, code)
	if err != nil {
		c.Log.WithError(err).Errorf("Failed to exchange oauth code into token")
		p.writeAPIError(w, &APIErrorResponse{Message: "failed to exchange oauth code into token", StatusCode: http.StatusInternalServerError})
		return
	}

	githubClient := p.githubConnectToken(*tok)
	gitUser, _, err := githubClient.Users.Get(ctx, "")
	if err != nil {
		c.Log.WithError(err).Errorf("Failed to get authenticated GitHub user")
		p.writeAPIError(w, &APIErrorResponse{Message: "failed to get authenticated GitHub user", StatusCode: http.StatusInternalServerError})
		return
	}

	// track the successful connection
	p.TrackUserEvent("account_connected", c.UserID, nil)

	userInfo := &GitHubUserInfo{
		UserID:         state.UserID,
		Token:          tok,
		GitHubUsername: gitUser.GetLogin(),
		LastToDoPostAt: model.GetMillis(),
		Settings: &UserSettings{
			SidebarButtons: settingButtonsTeam,
			DailyReminder:  true,
			Notifications:  true,
		},
		AllowedPrivateRepos:   state.PrivateAllowed,
		MM34646ResetTokenDone: true,
	}

	if err = p.storeGitHubUserInfo(userInfo); err != nil {
		c.Log.WithError(err).Errorf("Failed to store GitHub user info")
		p.writeAPIError(w, &APIErrorResponse{Message: "unable to connect user to GitHub", StatusCode: http.StatusInternalServerError})
		return
	}

	if err = p.storeGitHubToUserIDMapping(gitUser.GetLogin(), state.UserID); err != nil {
		c.Log.WithError(err).Warnf("Failed to store GitHub user info mapping")
	}

	flow := p.flowManager.setupFlow.ForUser(c.UserID)

	stepName, err := flow.GetCurrentStep()
	if err != nil {
		c.Log.WithError(err).Warnf("Failed to get current step")
	}

	if stepName == stepOAuthConnect {
		err = flow.Go(stepWebhookQuestion)
		if err != nil {
			c.Log.WithError(err).Warnf("Failed go to next step")
		}
	} else {
		// Only post introduction message if no setup wizard is running

		var commandHelp string
		commandHelp, err = renderTemplate("helpText", p.getConfiguration())
		if err != nil {
			c.Log.WithError(err).Warnf("Failed to render help template")
		}

		message := fmt.Sprintf("#### Welcome to the Mattermost GitHub Plugin!\n"+
			"You've connected your Mattermost account to [%s](%s) on GitHub. Read about the features of this plugin below:\n\n"+
			"##### Daily Reminders\n"+
			"The first time you log in each day, you'll get a post right here letting you know what messages you need to read and what pull requests are awaiting your review.\n"+
			"Turn off reminders with `/github settings reminders off`.\n\n"+
			"##### Notifications\n"+
			"When someone mentions you, requests your review, comments on or modifies one of your pull requests/issues, or assigns you, you'll get a post here about it.\n"+
			"Turn off notifications with `/github settings notifications off`.\n\n"+
			"##### Sidebar Buttons\n"+
			"Check out the buttons in the left-hand sidebar of Mattermost.\n"+
			"It shows your Open PRs, PRs that are awaiting your review, issues assigned to you, and all your unread messages you have in GitHub. \n"+
			"* The first button tells you how many pull requests you have submitted.\n"+
			"* The second shows the number of PR that are awaiting your review.\n"+
			"* The third shows the number of PR and issues your are assiged to.\n"+
			"* The fourth tracks the number of unread messages you have.\n"+
			"* The fifth will refresh the numbers.\n\n"+
			"Click on them!\n\n"+
			"##### Slash Commands\n"+
			commandHelp, gitUser.GetLogin(), gitUser.GetHTMLURL())

		p.CreateBotDMPost(state.UserID, message, "custom_git_welcome")
	}

	config := p.getConfiguration()
	orgList := p.configuration.getOrganizations()
	p.client.Frontend.PublishWebSocketEvent(
		wsEventConnect,
		map[string]interface{}{
			"connected":           true,
			"github_username":     userInfo.GitHubUsername,
			"github_client_id":    config.GitHubOAuthClientID,
			"enterprise_base_url": config.EnterpriseBaseURL,
			"organizations":       orgList,
			"configuration":       config.ClientConfiguration(),
		},
		&model.WebsocketBroadcast{UserId: state.UserID},
	)

	html := `
			<!DOCTYPE html>
			<html>
			<head>
			<script>
			window.close();
			</script>
			</head>
			<body>
			<p>Completed connecting to GitHub. Please close this window.</p>
			</body>
			</html>
			`

	w.Header().Set("Content-Type", "text/html")
	_, err = w.Write([]byte(html))
	if err != nil {
		c.Log.WithError(err).Errorf("Failed to write HTML response")
		p.writeAPIError(w, &APIErrorResponse{Message: "failed to write HTML response", StatusCode: http.StatusInternalServerError})
		return
	}
}

func (p *Plugin) getGitHubUser(c *Context, w http.ResponseWriter, r *http.Request) {
	type GitHubUserRequest struct {
		UserID string `json:"user_id"`
	}

	req := &GitHubUserRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.Log.WithError(err).Warnf("Error decoding GitHubUserRequest from JSON body")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a JSON object.", StatusCode: http.StatusBadRequest})
		return
	}

	if req.UserID == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a JSON object with a non-blank user_id field.", StatusCode: http.StatusBadRequest})
		return
	}

	userInfo, apiErr := p.getGitHubUserInfo(req.UserID)
	if apiErr != nil {
		if apiErr.ID == apiErrorIDNotConnected {
			p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "User is not connected to a GitHub account.", StatusCode: http.StatusNotFound})
		} else {
			p.writeAPIError(w, apiErr)
		}
		return
	}

	if userInfo == nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "User is not connected to a GitHub account.", StatusCode: http.StatusNotFound})
		return
	}

	type GitHubUserResponse struct {
		Username string `json:"username"`
	}

	resp := &GitHubUserResponse{Username: userInfo.GitHubUsername}
	p.writeJSON(w, resp)
}

func (p *Plugin) getConnected(c *Context, w http.ResponseWriter, r *http.Request) {
	config := p.getConfiguration()

	type ConnectedResponse struct {
		Connected           bool                   `json:"connected"`
		GitHubUsername      string                 `json:"github_username"`
		GitHubClientID      string                 `json:"github_client_id"`
		EnterpriseBaseURL   string                 `json:"enterprise_base_url,omitempty"`
		Organizations       []string               `json:"organizations"`
		UserSettings        *UserSettings          `json:"user_settings"`
		ClientConfiguration map[string]interface{} `json:"configuration"`
	}

	orgList := p.configuration.getOrganizations()
	resp := &ConnectedResponse{
		Connected:           false,
		EnterpriseBaseURL:   config.EnterpriseBaseURL,
		Organizations:       orgList,
		ClientConfiguration: p.getConfiguration().ClientConfiguration(),
	}

	if c.UserID == "" {
		p.writeJSON(w, resp)
		return
	}

	info, err := p.getGitHubUserInfo(c.UserID)
	if err != nil {
		c.Log.WithError(err).Errorf("failed to get GitHub user info")
		p.writeAPIError(w, &APIErrorResponse{Message: fmt.Sprintf("failed to get GitHub user info. %s", err.Message), StatusCode: err.StatusCode})
		return
	}

	if info == nil || info.Token == nil {
		p.writeJSON(w, resp)
		return
	}

	resp.Connected = true
	resp.GitHubUsername = info.GitHubUsername
	resp.GitHubClientID = config.GitHubOAuthClientID
	resp.UserSettings = info.Settings

	if info.Settings.DailyReminder && r.URL.Query().Get("reminder") == "true" {
		lastPostAt := info.LastToDoPostAt

		offset, err := strconv.Atoi(r.Header.Get("X-Timezone-Offset"))
		if err != nil {
			c.Log.WithError(err).Warnf("Invalid timezone offset")
			p.writeAPIError(w, &APIErrorResponse{Message: "invalid timezone offset", StatusCode: http.StatusBadRequest})
			return
		}

		timezone := time.FixedZone("local", -60*offset)
		// Post to do message if it's the next day and been more than an hour since the last post
		now := model.GetMillis()
		nt := time.Unix(now/1000, 0).In(timezone)
		lt := time.Unix(lastPostAt/1000, 0).In(timezone)
		if nt.Sub(lt).Hours() >= 1 && (nt.Day() != lt.Day() || nt.Month() != lt.Month() || nt.Year() != lt.Year()) {
			if p.HasUnreads(info) {
				if err := p.PostToDo(info, c.UserID); err != nil {
					c.Log.WithError(err).Warnf("Failed to create GitHub todo message")
				}
				info.LastToDoPostAt = now
				if err := p.storeGitHubUserInfo(info); err != nil {
					c.Log.WithError(err).Warnf("Failed to store github info for new user")
				}
			}
		}
	}

	privateRepoStoreKey := info.UserID + githubPrivateRepoKey
	if config.EnablePrivateRepo && !info.AllowedPrivateRepos {
		var val []byte
		err := p.store.Get(privateRepoStoreKey, &val)
		if err != nil {
			p.writeAPIError(w, &APIErrorResponse{Message: "Unable to get private repo key value", StatusCode: http.StatusInternalServerError})
			c.Log.WithError(err).Errorf("Unable to get private repo key value")
			return
		}

		// Inform the user once that private repositories enabled
		if val == nil {
			message := "Private repositories have been enabled for this plugin. To be able to use them you must disconnect and reconnect your GitHub account. To reconnect your account, use the following slash commands: `/github disconnect` followed by %s"
			if config.ConnectToPrivateByDefault {
				p.CreateBotDMPost(info.UserID, fmt.Sprintf(message, "`/github connect`."), "")
			} else {
				p.CreateBotDMPost(info.UserID, fmt.Sprintf(message, "`/github connect private`."), "")
			}
			if _, err := p.store.Set(privateRepoStoreKey, []byte("1")); err != nil {
				p.writeAPIError(w, &APIErrorResponse{Message: "unable to set private repo key value", StatusCode: http.StatusInternalServerError})
				c.Log.WithError(err).Errorf("Unable to set private repo key value")
			}
		}
	}

	p.writeJSON(w, resp)
}

func (p *Plugin) getMentions(c *UserContext, w http.ResponseWriter, r *http.Request) {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	username := c.GHInfo.GitHubUsername
	orgList := p.configuration.getOrganizations()
	query := getMentionSearchQuery(username, orgList)

	var result *github.IssuesSearchResult
	var err error
	cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
		result, _, err = githubClient.Search.Issues(c.Ctx, query, &github.SearchOptions{})
		if err != nil {
			return err
		}
		return nil
	})
	if cErr != nil {
		p.writeAPIError(w, &APIErrorResponse{Message: "failed to search for issues", StatusCode: http.StatusInternalServerError})
		c.Log.WithError(cErr).With(logger.LogContext{"query": query}).Errorf("Failed to search for issues")
		return
	}

	p.writeJSON(w, result.Issues)
}

func (p *Plugin) getUnreadsData(c *UserContext) []*FilteredNotification {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	var notifications []*github.Notification
	var err error
	cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
		notifications, _, err = githubClient.Activity.ListNotifications(c.Ctx, &github.NotificationListOptions{})
		if err != nil {
			return err
		}
		return nil
	})
	if cErr != nil {
		c.Log.WithError(cErr).Warnf("Failed to list notifications")
		return nil
	}

	filteredNotifications := []*FilteredNotification{}
	for _, n := range notifications {
		if n.GetReason() == notificationReasonSubscribed {
			continue
		}

		if p.checkOrg(n.GetRepository().GetOwner().GetLogin()) != nil {
			continue
		}

		issueURL := n.GetSubject().GetURL()
		issueNumIndex := strings.LastIndex(issueURL, "/")
		issueNum := issueURL[issueNumIndex+1:]
		subjectURL := n.GetSubject().GetURL()
		if n.GetSubject().GetLatestCommentURL() != "" {
			subjectURL = n.GetSubject().GetLatestCommentURL()
		}

		filteredNotifications = append(filteredNotifications, &FilteredNotification{
			Notification: *n,
			HTMLURL:      fixGithubNotificationSubjectURL(subjectURL, issueNum),
		})
	}

	return filteredNotifications
}

func (p *Plugin) getPrsDetails(c *UserContext, w http.ResponseWriter, r *http.Request) {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)

	var prList []*PRDetails
	if err := json.NewDecoder(r.Body).Decode(&prList); err != nil {
		c.Log.WithError(err).Warnf("Error decoding PRDetails JSON body")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a JSON object.", StatusCode: http.StatusBadRequest})
		return
	}

	prDetails := make([]*PRDetails, len(prList))
	var wg sync.WaitGroup
	for i, pr := range prList {
		i := i
		pr := pr
		wg.Add(1)
		go func() {
			defer wg.Done()
			prDetail := p.fetchPRDetails(c, githubClient, pr.URL, pr.Number)
			prDetails[i] = prDetail
		}()
	}

	wg.Wait()

	p.writeJSON(w, prDetails)
}

func (p *Plugin) fetchPRDetails(c *UserContext, client *github.Client, prURL string, prNumber int) *PRDetails {
	var status string
	var mergeable bool
	// Initialize to a non-nil slice to simplify JSON handling semantics
	requestedReviewers := []*string{}
	reviewsList := []*github.PullRequestReview{}

	repoOwner, repoName := getRepoOwnerAndNameFromURL(prURL)

	var wg sync.WaitGroup

	// Fetch reviews
	wg.Add(1)
	go func() {
		defer wg.Done()
		fetchedReviews, err := fetchReviews(c, client, repoOwner, repoName, prNumber)
		if err != nil {
			c.Log.WithError(err).Warnf("Failed to fetch reviews for PR details")
			return
		}
		reviewsList = fetchedReviews
	}()

	// Fetch reviewers and status
	wg.Add(1)
	go func() {
		defer wg.Done()
		prInfo, _, err := client.PullRequests.Get(c.Ctx, repoOwner, repoName, prNumber)
		if err != nil {
			c.Log.WithError(err).Warnf("Failed to fetch PR for PR details")
			return
		}

		mergeable = prInfo.GetMergeable()

		for _, v := range prInfo.RequestedReviewers {
			requestedReviewers = append(requestedReviewers, v.Login)
		}
		statuses, _, err := client.Repositories.GetCombinedStatus(c.Ctx, repoOwner, repoName, prInfo.GetHead().GetSHA(), nil)
		if err != nil {
			c.Log.WithError(err).Warnf("Failed to fetch combined status")
			return
		}
		status = *statuses.State
	}()

	wg.Wait()
	return &PRDetails{
		URL:                prURL,
		Number:             prNumber,
		Status:             status,
		Mergeable:          mergeable,
		RequestedReviewers: requestedReviewers,
		Reviews:            reviewsList,
	}
}

func fetchReviews(c *UserContext, client *github.Client, repoOwner string, repoName string, number int) ([]*github.PullRequestReview, error) {
	reviewsList, _, err := client.PullRequests.ListReviews(c.Ctx, repoOwner, repoName, number, nil)

	if err != nil {
		return []*github.PullRequestReview{}, errors.Wrap(err, "could not list reviews")
	}

	return reviewsList, nil
}

func getRepoOwnerAndNameFromURL(url string) (string, string) {
	splitted := strings.Split(url, "/")
	return splitted[len(splitted)-2], splitted[len(splitted)-1]
}

func (p *Plugin) searchIssues(c *UserContext, w http.ResponseWriter, r *http.Request) {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)

	searchTerm := r.FormValue("term")
	orgsList := p.configuration.getOrganizations()
	allIssues := []*github.Issue{}

	if len(orgsList) == 0 {
		orgsList = []string{""}
	}

	hasFetchedIssues := false
	for _, org := range orgsList {
		query := getIssuesSearchQuery(org, searchTerm)
		var result *github.IssuesSearchResult
		var err error
		cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
			result, _, err = githubClient.Search.Issues(c.Ctx, query, &github.SearchOptions{})
			if err != nil {
				return err
			}
			return nil
		})
		if cErr != nil {
			c.Log.WithError(cErr).With(logger.LogContext{"query": query}).Warnf("Failed to search for issues")
		}

		if result != nil && len(result.Issues) > 0 {
			allIssues = append(allIssues, result.Issues...)
			hasFetchedIssues = true
		}
	}

	if !hasFetchedIssues {
		p.writeJSON(w, make([]*github.Issue, 0))
		return
	}

	p.writeJSON(w, allIssues)
}

func (p *Plugin) getPermaLink(postID string) (string, error) {
	siteURL, err := getSiteURL(p.client)
	if err != nil {
		return "", err
	}

	redirectURL, err := url.JoinPath(siteURL, "_redirect", "pl", postID)
	if err != nil {
		return "", errors.Wrap(err, "failed to build pluginURL")
	}

	return redirectURL, nil
}

func getFailReason(code int, repo string, username string) string {
	cause := ""
	switch code {
	case http.StatusInternalServerError:
		cause = "Internal server error"
	case http.StatusBadRequest:
		cause = "Bad request"
	case http.StatusNotFound:
		cause = fmt.Sprintf("Sorry, either you don't have access to the repo %s with the user %s or it is no longer available", repo, username)
	case http.StatusUnauthorized:
		cause = fmt.Sprintf("Sorry, your user %s is unauthorized to do this action", username)
	case http.StatusForbidden:
		cause = fmt.Sprintf("Sorry, you don't have enough permissions to comment in the repo %s with the user %s", repo, username)
	default:
		cause = fmt.Sprintf("Unknown status code %d", code)
	}
	return cause
}

func (p *Plugin) createIssueComment(c *UserContext, w http.ResponseWriter, r *http.Request) {
	type CreateIssueCommentRequest struct {
		PostID  string `json:"post_id"`
		Owner   string `json:"owner"`
		Repo    string `json:"repo"`
		Number  int    `json:"number"`
		Comment string `json:"comment"`
	}

	req := &CreateIssueCommentRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.Log.WithError(err).Warnf("Error decoding CreateIssueCommentRequest JSON body")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a JSON object.", StatusCode: http.StatusBadRequest})
		return
	}

	if req.PostID == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid post id", StatusCode: http.StatusBadRequest})
		return
	}

	if req.Owner == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid repo owner.", StatusCode: http.StatusBadRequest})
		return
	}

	if req.Repo == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid repo.", StatusCode: http.StatusBadRequest})
		return
	}

	if req.Number == 0 {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid issue number.", StatusCode: http.StatusBadRequest})
		return
	}

	if req.Comment == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid non empty comment.", StatusCode: http.StatusBadRequest})
		return
	}

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)

	post, err := p.client.Post.GetPost(req.PostID)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to load post " + req.PostID, StatusCode: http.StatusInternalServerError})
		return
	}
	if post == nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to load post " + req.PostID + ": not found", StatusCode: http.StatusNotFound})
		return
	}

	commentUsername, err := p.getUsername(post.UserId)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to get username", StatusCode: http.StatusInternalServerError})
		return
	}

	currentUsername := c.GHInfo.GitHubUsername
	permalink, err := p.getPermaLink(req.PostID)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to generate permalink", StatusCode: http.StatusInternalServerError})
		return
	}
	permalinkMessage := fmt.Sprintf("*@%s attached a* [message](%s) *from %s*\n\n", currentUsername, permalink, commentUsername)

	req.Comment = permalinkMessage + req.Comment
	comment := &github.IssueComment{
		Body: &req.Comment,
	}

	var result *github.IssueComment
	var rawResponse *github.Response
	if cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
		result, rawResponse, err = githubClient.Issues.CreateComment(c.Ctx, req.Owner, req.Repo, req.Number, comment)
		if err != nil {
			return err
		}
		return nil
	}); cErr != nil {
		statusCode := 500
		if rawResponse != nil {
			statusCode = rawResponse.StatusCode
		}
		c.Log.WithError(err).With(logger.LogContext{
			"owner":  req.Owner,
			"repo":   req.Repo,
			"number": req.Number,
		}).Errorf("failed to create an issue comment")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to create an issue comment: " + getFailReason(statusCode, req.Repo, currentUsername), StatusCode: statusCode})
		return
	}

	rootID := req.PostID
	if post.RootId != "" {
		// the original post was a reply
		rootID = post.RootId
	}

	permalinkReplyMessage := fmt.Sprintf("[Message](%v) attached to GitHub issue [#%v](%v)", permalink, req.Number, result.GetHTMLURL())
	reply := &model.Post{
		Message:   permalinkReplyMessage,
		ChannelId: post.ChannelId,
		RootId:    rootID,
		UserId:    c.UserID,
	}

	err = p.client.Post.CreatePost(reply)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to create notification post " + req.PostID, StatusCode: http.StatusInternalServerError})
		return
	}

	p.writeJSON(w, result)
}

func (p *Plugin) getLHSData(c *UserContext) (reviewResp []*graphql.GithubPRDetails, assignmentResp []*github.Issue, openPRResp []*graphql.GithubPRDetails, err error) {
	graphQLClient := p.graphQLConnect(c.GHInfo)

	reviewResp, assignmentResp, openPRResp, err = graphQLClient.GetLHSData(c.Context.Ctx)
	if err != nil {
		return []*graphql.GithubPRDetails{}, []*github.Issue{}, []*graphql.GithubPRDetails{}, err
	}

	return reviewResp, assignmentResp, openPRResp, nil
}

func (p *Plugin) getSidebarData(c *UserContext) (*SidebarContent, error) {
	reviewResp, assignmentResp, openPRResp, err := p.getLHSData(c)
	if err != nil {
		return nil, err
	}

	return &SidebarContent{
		PRs:         openPRResp,
		Assignments: assignmentResp,
		Reviews:     reviewResp,
		Unreads:     p.getUnreadsData(c),
	}, nil
}

func (p *Plugin) getSidebarContent(c *UserContext, w http.ResponseWriter, r *http.Request) {
	sidebarContent, err := p.getSidebarData(c)
	if err != nil {
		c.Log.WithError(err).Errorf("Failed to search for the sidebar data")
		p.writeAPIError(w, &APIErrorResponse{Message: "failed to search for the sidebar data", StatusCode: http.StatusInternalServerError})
		return
	}

	p.writeJSON(w, sidebarContent)
}

func (p *Plugin) postToDo(c *UserContext, w http.ResponseWriter, r *http.Request) {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)

	text, err := p.GetToDo(c.Ctx, c.GHInfo, githubClient)
	if err != nil {
		c.Log.WithError(err).Warnf("Failed to get Todos")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Encountered an error getting the to do items.", StatusCode: http.StatusUnauthorized})
		return
	}

	p.CreateBotDMPost(c.UserID, text, "custom_git_todo")

	resp := struct {
		Status string
	}{"OK"}

	p.writeJSON(w, resp)
}

func (p *Plugin) updateSettings(c *UserContext, w http.ResponseWriter, r *http.Request) {
	var settings *UserSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		c.Log.WithError(err).Warnf("Error decoding settings from JSON body")
		p.writeAPIError(w, &APIErrorResponse{Message: "invalid request body", StatusCode: http.StatusBadRequest})
		return
	}

	if settings == nil {
		p.client.Log.Error("Invalid request body.")
		p.writeAPIError(w, &APIErrorResponse{Message: "invalid request body", StatusCode: http.StatusBadRequest})
		return
	}

	info := c.GHInfo
	info.Settings = settings

	if err := p.storeGitHubUserInfo(info); err != nil {
		c.Log.WithError(err).Errorf("Failed to store GitHub user info")
		p.writeAPIError(w, &APIErrorResponse{Message: "error occurred while updating settings", StatusCode: http.StatusInternalServerError})
		return
	}

	p.writeJSON(w, info.Settings)
}

func (p *Plugin) getIssueByNumber(c *UserContext, w http.ResponseWriter, r *http.Request) {
	owner := r.FormValue("owner")
	repo := r.FormValue("repo")
	number := r.FormValue("number")
	numberInt, err := strconv.Atoi(number)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{Message: "Invalid param 'number'.", StatusCode: http.StatusBadRequest})
		return
	}

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)

	var result *github.Issue
	if cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
		result, _, err = githubClient.Issues.Get(c.Ctx, owner, repo, numberInt)
		if err != nil {
			return err
		}
		return nil
	}); cErr != nil {
		// If the issue is not found, it's probably behind a private repo.
		// Return an empty response in this case.
		var gerr *github.ErrorResponse
		if errors.As(cErr, &gerr) && gerr.Response.StatusCode == http.StatusNotFound {
			c.Log.WithError(err).With(logger.LogContext{
				"owner":  owner,
				"repo":   repo,
				"number": numberInt,
			}).Debugf("Issue  not found")
			p.writeJSON(w, nil)
			return
		}

		c.Log.WithError(cErr).With(logger.LogContext{
			"owner":  owner,
			"repo":   repo,
			"number": numberInt,
		}).Errorf("Could not get issue")
		p.writeAPIError(w, &APIErrorResponse{Message: "Could not get issue", StatusCode: http.StatusInternalServerError})
		return
	}
	if result.Body != nil {
		*result.Body = mdCommentRegex.ReplaceAllString(result.GetBody(), "")
	}
	p.writeJSON(w, result)
}

func (p *Plugin) getPrByNumber(c *UserContext, w http.ResponseWriter, r *http.Request) {
	owner := r.FormValue("owner")
	repo := r.FormValue("repo")
	number := r.FormValue("number")

	numberInt, err := strconv.Atoi(number)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{Message: "Invalid param 'number'.", StatusCode: http.StatusBadRequest})
		return
	}

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	var result *github.PullRequest
	if cErr := p.useGitHubClient(c.GHInfo, func(userInfo *GitHubUserInfo, token *oauth2.Token) error {
		result, _, err = githubClient.PullRequests.Get(c.Ctx, owner, repo, numberInt)
		if err != nil {
			return err
		}
		return nil
	}); cErr != nil {
		// If the pull request is not found, it's probably behind a private repo.
		// Return an empty repose in this case.
		var gerr *github.ErrorResponse
		if errors.As(cErr, &gerr) && gerr.Response.StatusCode == http.StatusNotFound {
			c.Log.With(logger.LogContext{
				"owner":  owner,
				"repo":   repo,
				"number": numberInt,
			}).Debugf("Pull request not found")

			p.writeJSON(w, nil)
			return
		}

		c.Log.WithError(cErr).With(logger.LogContext{
			"owner":  owner,
			"repo":   repo,
			"number": numberInt,
		}).Errorf("Could not get pull request")
		p.writeAPIError(w, &APIErrorResponse{Message: "Could not get pull request", StatusCode: http.StatusInternalServerError})
		return
	}
	if result.Body != nil {
		*result.Body = mdCommentRegex.ReplaceAllString(result.GetBody(), "")
	}
	p.writeJSON(w, result)
}

func (p *Plugin) getLabels(c *UserContext, w http.ResponseWriter, r *http.Request) {
	owner, repo, err := parseRepo(r.URL.Query().Get("repo"))
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{Message: err.Error(), StatusCode: http.StatusBadRequest})
		return
	}

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	var allLabels []*github.Label
	opt := github.ListOptions{PerPage: 50}

	for {
		var labels []*github.Label
		var resp *github.Response
		if cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
			labels, resp, err = githubClient.Issues.ListLabels(c.Ctx, owner, repo, &opt)
			if err != nil {
				return err
			}
			return nil
		}); cErr != nil {
			c.Log.WithError(cErr).With(logger.LogContext{
				"owner": owner,
				"repo":  repo,
			}).Errorf("Failed to list labels")
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch labels", StatusCode: http.StatusInternalServerError})
			return
		}
		allLabels = append(allLabels, labels...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	p.writeJSON(w, allLabels)
}

func (p *Plugin) getAssignees(c *UserContext, w http.ResponseWriter, r *http.Request) {
	owner, repo, err := parseRepo(r.URL.Query().Get("repo"))
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{Message: err.Error(), StatusCode: http.StatusBadRequest})
		return
	}

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	var allAssignees []*github.User
	opt := github.ListOptions{PerPage: 50}

	for {
		var assignees []*github.User
		var resp *github.Response
		if cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
			assignees, resp, err = githubClient.Issues.ListAssignees(c.Ctx, owner, repo, &opt)
			if err != nil {
				return err
			}
			return nil
		}); cErr != nil {
			c.Log.WithError(cErr).With(logger.LogContext{
				"owner": owner,
				"repo":  repo,
			}).Errorf("Failed to list assignees")
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch assignees", StatusCode: http.StatusInternalServerError})
			return
		}
		allAssignees = append(allAssignees, assignees...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	p.writeJSON(w, allAssignees)
}

func (p *Plugin) getMilestones(c *UserContext, w http.ResponseWriter, r *http.Request) {
	owner, repo, err := parseRepo(r.URL.Query().Get("repo"))
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{Message: err.Error(), StatusCode: http.StatusBadRequest})
		return
	}

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	var allMilestones []*github.Milestone
	opt := github.ListOptions{PerPage: 50}

	for {
		var milestones []*github.Milestone
		var resp *github.Response
		if cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
			milestones, resp, err = githubClient.Issues.ListMilestones(c.Ctx, owner, repo, &github.MilestoneListOptions{ListOptions: opt})
			if err != nil {
				return err
			}
			return nil
		}); cErr != nil {
			c.Log.WithError(cErr).With(logger.LogContext{
				"owner": owner,
				"repo":  repo,
			}).Errorf("Failed to list milestones")
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch milestones", StatusCode: http.StatusInternalServerError})
			return
		}
		allMilestones = append(allMilestones, milestones...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	p.writeJSON(w, allMilestones)
}

func getOrganizationList(c context.Context, userName string, githubClient *github.Client, opt github.ListOptions) ([]*github.Organization, error) {
	var allOrgs []*github.Organization
	for {
		orgs, resp, err := githubClient.Organizations.List(c, userName, &opt)
		if err != nil {
			return nil, err
		}

		allOrgs = append(allOrgs, orgs...)
		if resp.NextPage == 0 {
			break
		}

		opt.Page = resp.NextPage
	}

	return allOrgs, nil
}

func (p *Plugin) getRepositoryList(c context.Context, ghInfo *GitHubUserInfo, userName string, githubClient *github.Client, opt github.ListOptions) ([]*github.Repository, error) {
	var allRepos []*github.Repository
	for {
		var repos []*github.Repository
		var resp *github.Response
		var err error
		cErr := p.useGitHubClient(ghInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
			repos, resp, err = githubClient.Repositories.List(c, userName, &github.RepositoryListOptions{ListOptions: opt})
			if err != nil {
				return err
			}
			return nil
		})
		if cErr != nil {
			return nil, cErr
		}

		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}

		opt.Page = resp.NextPage
	}

	return allRepos, nil
}

func (p *Plugin) getRepositoryListByOrg(c context.Context, ghInfo *GitHubUserInfo, org string, githubClient *github.Client, opt github.ListOptions) ([]*github.Repository, int, error) {
	var allRepos []*github.Repository
	for {
		var repos []*github.Repository
		var resp *github.Response
		var err error
		cErr := p.useGitHubClient(ghInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
			repos, resp, err = githubClient.Repositories.ListByOrg(c, org, &github.RepositoryListByOrgOptions{Sort: "full_name", ListOptions: opt})
			if err != nil {
				return err
			}
			return nil
		})
		if cErr != nil {
			return nil, resp.StatusCode, cErr
		}

		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	return allRepos, http.StatusOK, nil
}

func (p *Plugin) getOrganizations(c *UserContext, w http.ResponseWriter, r *http.Request) {
	var allOrgs []*github.Organization
	org := p.getConfiguration().GitHubOrg

	if org == "" {
		includeLoggedInUser := r.URL.Query().Get("includeLoggedInUser")
		if includeLoggedInUser == "true" {
			allOrgs = append(allOrgs, &github.Organization{Login: &c.GHInfo.GitHubUsername})
		}
		githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
		orgList, err := getOrganizationList(c.Ctx, "", githubClient, github.ListOptions{PerPage: 50})
		if err != nil {
			c.Log.WithError(err).Errorf("Failed to list organizations")
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch organizations", StatusCode: http.StatusInternalServerError})
			return
		}
		allOrgs = append(allOrgs, orgList...)
	} else {
		allOrgs = append(allOrgs, &github.Organization{Login: &org})
	}
	// Only send required organizations to the client
	type OrganizationResponse struct {
		Login string `json:"login,omitempty"`
	}

	resp := make([]*OrganizationResponse, len(allOrgs))
	for i, r := range allOrgs {
		resp[i] = &OrganizationResponse{
			Login: r.GetLogin(),
		}
	}

	p.writeJSON(w, resp)
}

func (p *Plugin) getReposByOrg(c *UserContext, w http.ResponseWriter, r *http.Request) {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)

	opt := github.ListOptions{PerPage: 50}

	org := r.URL.Query().Get("organization")

	if org == "" {
		c.Log.Warnf("Organization query param is empty")
		p.writeAPIError(w, &APIErrorResponse{Message: "Organization query is empty, must include organization name ", StatusCode: http.StatusBadRequest})
		return
	}

	var allRepos []*github.Repository
	var err error
	var statusCode int

	// If an organization is the username of an authenticated user then return repos where the authenticated user is the owner
	if org == c.GHInfo.GitHubUsername {
		allRepos, err = p.getRepositoryList(c.Ctx, c.GHInfo, "", githubClient, opt)
		if err != nil {
			c.Log.WithError(err).Errorf("Failed to list repositories")
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch repositories", StatusCode: http.StatusInternalServerError})
			return
		}
	} else {
		allRepos, statusCode, err = p.getRepositoryListByOrg(c.Ctx, c.GHInfo, org, githubClient, opt)
		if err != nil {
			if statusCode == http.StatusNotFound {
				allRepos, err = p.getRepositoryList(c.Ctx, c.GHInfo, org, githubClient, opt)
				if err != nil {
					c.Log.WithError(err).Errorf("Failed to list repositories")
					p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch repositories", StatusCode: http.StatusInternalServerError})
					return
				}
			} else {
				c.Log.WithError(err).Warnf("Failed to list repositories")
				p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch repositories", StatusCode: statusCode})
				return
			}
		}
	}
	// Only send repositories which are part of the requested organization
	type RepositoryResponse struct {
		Name        string          `json:"name,omitempty"`
		FullName    string          `json:"full_name,omitempty"`
		Permissions map[string]bool `json:"permissions,omitempty"`
	}

	resp := make([]*RepositoryResponse, len(allRepos))
	for i, r := range allRepos {
		resp[i] = &RepositoryResponse{
			Name:        r.GetName(),
			FullName:    r.GetFullName(),
			Permissions: r.GetPermissions(),
		}
	}

	p.writeJSON(w, resp)
}

func getRepository(c context.Context, org string, repo string, githubClient *github.Client) (*github.Repository, error) {
	repository, _, err := githubClient.Repositories.Get(c, org, repo)
	if err != nil {
		return nil, err
	}

	return repository, nil
}

func (p *Plugin) getRepositories(c *UserContext, w http.ResponseWriter, r *http.Request) {
	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	org := p.getConfiguration().GitHubOrg

	channelID := r.URL.Query().Get(channelIDParam)
	if channelID == "" {
		p.client.Log.Warn("Bad request: missing channelId")
		p.writeAPIError(w, &APIErrorResponse{Message: "Bad request: missing channelId", StatusCode: http.StatusBadRequest})
		return
	}

	var allRepos []*github.Repository
	var err error

	opt := github.ListOptions{PerPage: 50}

	if org == "" {
		allRepos, err = p.getRepositoryList(c.Ctx, c.GHInfo, "", githubClient, opt)
		if err != nil {
			c.Log.WithError(err).Warnf("Failed to list repositories")
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch repositories", StatusCode: http.StatusInternalServerError})
			return
		}
	} else {
		orgsList := p.configuration.getOrganizations()
		hasFetchedRepos := false
		for _, org := range orgsList {
			orgRepos, statusCode, err := p.getRepositoryListByOrg(c.Ctx, c.GHInfo, org, githubClient, opt)
			if err != nil {
				if statusCode == http.StatusNotFound {
					orgRepos, err = p.getRepositoryList(c.Ctx, c.GHInfo, org, githubClient, opt)
					if err != nil {
						c.Log.WithError(err).Warnf("Failed to list repositories", "Organization", org)
					}
				} else {
					c.Log.WithError(err).Warnf("Failed to list repositories", "Organization", org)
				}
			}

			if len(orgRepos) > 0 {
				allRepos = append(allRepos, orgRepos...)
				hasFetchedRepos = true
			}
		}

		if !hasFetchedRepos {
			p.writeAPIError(w, &APIErrorResponse{Message: "Failed to fetch repositories", StatusCode: http.StatusInternalServerError})
			return
		}
	}

	repoResp := make([]RepoResponse, len(allRepos))
	for i, r := range allRepos {
		repoResp[i].Name = r.GetName()
		repoResp[i].FullName = r.GetFullName()
		repoResp[i].Permissions = r.GetPermissions()
	}

	resp := RepositoryResponse{
		Repos: repoResp,
	}

	defaultRepo, dErr := p.GetDefaultRepo(c.GHInfo.UserID, channelID)
	if dErr != nil {
		c.Log.WithError(dErr).Warnf("Failed to get the default repo for the channel. UserID: %s. ChannelID: %s", c.GHInfo.UserID, channelID)
	}

	if defaultRepo != "" {
		config := p.getConfiguration()
		baseURL := config.getBaseURL()
		owner, repo := parseOwnerAndRepo(defaultRepo, baseURL)
		defaultRepository, err := getRepository(c.Ctx, owner, repo, githubClient)
		if err != nil {
			c.Log.WithError(err).Warnf("Failed to get the default repo %s/%s", owner, repo)
		}

		if defaultRepository != nil {
			resp.DefaultRepo = RepoResponse{
				Name:        *defaultRepository.Name,
				FullName:    *defaultRepository.FullName,
				Permissions: defaultRepository.Permissions,
			}
		}
	}

	p.writeJSON(w, resp)
}

func (p *Plugin) createIssue(c *UserContext, w http.ResponseWriter, r *http.Request) {
	type IssueRequest struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Repo      string   `json:"repo"`
		PostID    string   `json:"post_id"`
		ChannelID string   `json:"channel_id"`
		Labels    []string `json:"labels"`
		Assignees []string `json:"assignees"`
		Milestone int      `json:"milestone"`
	}

	// get data for the issue from the request body and fill IssueRequest object
	issue := &IssueRequest{}

	if err := json.NewDecoder(r.Body).Decode(&issue); err != nil {
		c.Log.WithError(err).Warnf("Error decoding JSON body")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a JSON object.", StatusCode: http.StatusBadRequest})
		return
	}

	if issue.Title == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid issue title.", StatusCode: http.StatusBadRequest})
		return
	}

	if issue.Repo == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide a valid repo name.", StatusCode: http.StatusBadRequest})
		return
	}

	if issue.PostID == "" && issue.ChannelID == "" {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Please provide either a postID or a channelID", StatusCode: http.StatusBadRequest})
		return
	}

	mmMessage := ""
	var post *model.Post
	permalink := ""
	if issue.PostID != "" {
		var err error
		post, err = p.client.Post.GetPost(issue.PostID)
		if err != nil {
			p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to load post " + issue.PostID, StatusCode: http.StatusInternalServerError})
			return
		}
		if post == nil {
			p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to load post " + issue.PostID + ": not found", StatusCode: http.StatusNotFound})
			return
		}

		username, err := p.getUsername(post.UserId)
		if err != nil {
			p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to get username", StatusCode: http.StatusInternalServerError})
			return
		}

		permalink, err = p.getPermaLink(issue.PostID)
		if err != nil {
			p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to generate permalink", StatusCode: http.StatusInternalServerError})
			return
		}

		mmMessage = fmt.Sprintf("_Issue created from a [Mattermost message](%v) *by %s*._", permalink, username)
	}

	ghIssue := &github.IssueRequest{
		Title:     &issue.Title,
		Body:      &issue.Body,
		Labels:    &issue.Labels,
		Assignees: &issue.Assignees,
	}

	// submitting the request with an invalid milestone ID results in a 422 error
	// we make sure it's not zero here, because the webapp client might have left this field empty
	if issue.Milestone > 0 {
		ghIssue.Milestone = &issue.Milestone
	}

	if ghIssue.GetBody() != "" && mmMessage != "" {
		mmMessage = "\n\n" + mmMessage
	}
	*ghIssue.Body = ghIssue.GetBody() + mmMessage

	currentUser, err := p.client.User.Get(c.UserID)
	if err != nil {
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to load current user", StatusCode: http.StatusInternalServerError})
		return
	}

	splittedRepo := strings.Split(issue.Repo, "/")
	owner := splittedRepo[0]
	repoName := splittedRepo[1]

	githubClient := p.githubConnectUser(c.Context.Ctx, c.GHInfo)
	var resp *github.Response
	var result *github.Issue
	if cErr := p.useGitHubClient(c.GHInfo, func(info *GitHubUserInfo, token *oauth2.Token) error {
		result, resp, err = githubClient.Issues.Create(c.Ctx, owner, repoName, ghIssue)
		if err != nil {
			return err
		}
		return nil
	}); cErr != nil {
		if resp != nil && resp.Response.StatusCode == http.StatusGone {
			p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "Issues are disabled on this repository.", StatusCode: http.StatusMethodNotAllowed})
			return
		}

		c.Log.WithError(cErr).Warnf("Failed to create issue")
		p.writeAPIError(w,
			&APIErrorResponse{
				ID: "",
				Message: "failed to create issue: " + getFailReason(resp.StatusCode,
					issue.Repo,
					currentUser.Username,
				),
				StatusCode: resp.StatusCode,
			})
		return
	}

	rootID := issue.PostID
	channelID := issue.ChannelID
	message := fmt.Sprintf("Created GitHub issue [#%v](%v)", result.GetNumber(), result.GetHTMLURL())
	if post != nil {
		if post.RootId != "" {
			rootID = post.RootId
		}
		channelID = post.ChannelId
		message += fmt.Sprintf(" from a [message](%s)", permalink)
	}

	reply := &model.Post{
		Message:   message,
		ChannelId: channelID,
		RootId:    rootID,
		UserId:    c.UserID,
	}

	if post != nil {
		err = p.client.Post.CreatePost(reply)
	} else {
		p.client.Post.SendEphemeralPost(c.UserID, reply)
	}
	if err != nil {
		c.Log.WithError(err).Errorf("failed to create notification post")
		p.writeAPIError(w, &APIErrorResponse{ID: "", Message: "failed to create notification post, postID: " + issue.PostID + ", channelID: " + channelID, StatusCode: http.StatusInternalServerError})
		return
	}

	p.writeJSON(w, result)
}

func (p *Plugin) getConfig(w http.ResponseWriter, r *http.Request) {
	config := p.getConfiguration()

	p.writeJSON(w, config)
}

func (p *Plugin) getToken(w http.ResponseWriter, r *http.Request) {
	userID := r.FormValue("userID")
	if userID == "" {
		p.client.Log.Error("UserID not found.")
		p.writeAPIError(w, &APIErrorResponse{Message: "please provide a userID", StatusCode: http.StatusBadRequest})
		return
	}

	info, apiErr := p.getGitHubUserInfo(userID)
	if apiErr != nil {
		p.client.Log.Error("error occurred while getting the github user info", "UserID", userID, "error", apiErr)
		p.writeAPIError(w, &APIErrorResponse{Message: apiErr.Error(), StatusCode: apiErr.StatusCode})
		return
	}

	p.writeJSON(w, info.Token)
}

// parseRepo parses the owner & repository name from the repo query parameter
func parseRepo(repoParam string) (owner, repo string, err error) {
	if repoParam == "" {
		return "", "", errors.New("repository cannot be blank")
	}

	splitted := strings.Split(repoParam, "/")
	if len(splitted) != 2 {
		return "", "", errors.New("invalid repository")
	}

	return splitted[0], splitted[1], nil
}
