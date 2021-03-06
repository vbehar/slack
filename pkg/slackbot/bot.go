package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jenkins-x/jx/v2/pkg/prow"
	"github.com/jenkins-x/lighthouse/pkg/apis/lighthouse/v1alpha1"
	"github.com/jenkins-x/lighthouse/pkg/jx"
	"github.com/jenkins-x/lighthouse/pkg/record"

	"k8s.io/apimachinery/pkg/types"

	"github.com/jenkins-x/jx/v2/pkg/users"

	slackapp "github.com/jenkins-x/slack/pkg/apis/slack/v1alpha1"

	"github.com/pkg/errors"

	jenkinsv1 "github.com/jenkins-x/jx/v2/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/v2/pkg/kube"

	"github.com/jenkins-x/jx/v2/pkg/gits"

	"github.com/jenkins-x/jx-logging/pkg/log"
	"github.com/jenkins-x/jx/v2/pkg/util"
	"github.com/slack-go/slack"
)

/*
"k8s.io/apimachinery/pkg/types"
*/
const (
	SlackAnnotationPrefix        = "bot.slack.apps.jenkins-x.io"
	pullRequestReviewMessageType = "pr"
	pipelineMessageType          = "pipeline"
)

var knownPipelineStageTypes = []string{"setup", "setVersion", "preBuild", "build", "postBuild", "promote", "pipeline"}

var defaultStatuses = slackapp.Statuses{
	Merged: &slackapp.Status{
		Emoji: ":purple_heart:",
		Text:  "merged",
	},
	Closed: &slackapp.Status{
		Emoji: ":closed_book:",
		Text:  "closed and not merged",
	},
	Aborted: &slackapp.Status{
		Emoji: ":red_circle:",
		Text:  "build aborted",
	},
	Errored: &slackapp.Status{
		Emoji: ":red_circle:",
		Text:  "build errored",
	},
	Failed: &slackapp.Status{
		Emoji: ":red_circle:",
		Text:  "build failed",
	},
	Approved: &slackapp.Status{
		Emoji: ":+1:",
		Text:  "approved",
	},
	NotApproved: &slackapp.Status{
		Emoji: ":wave:",
		Text:  "not approved",
	},
	NeedsOkToTest: &slackapp.Status{
		Emoji: ":wave:",
		Text:  "needs /ok-to-test",
	},
	Hold: &slackapp.Status{
		Emoji: ":octagonal_sign:",
		Text:  "hold",
	},
	Pending: &slackapp.Status{
		Emoji: ":question:",
		Text:  "build pending",
	},
	Running: &slackapp.Status{
		Emoji: ":white_circle:",
		Text:  "build running",
	},
	Succeeded: &slackapp.Status{
		Emoji: ":white_check_mark:",
		Text:  "build succeeded",
	},
	LGTM: &slackapp.Status{
		Emoji: ":+1:",
		Text:  "lgtm",
	},
	Unknown: &slackapp.Status{
		Emoji: ":grey_question:",
		Text:  "",
	},
}

type MessageReference struct {
	ChannelID string
	Timestamp string
}

func (o *SlackBotOptions) isEnabled(activity *record.ActivityRecord, orgs []slackapp.Org,
	ignoreLabels []string) (bool, *gits.GitPullRequest, *users.GitUserResolver, error) {
	if len(orgs) > 0 {
		found := false
		for _, o := range orgs {
			if o.Name == activity.Owner {
				if len(o.Repos) == 0 {
					found = true
					break
				}
				for _, r := range o.Repos {
					if r == activity.Repo {
						found = true
						break
					}
				}
			}
		}
		if !found {
			return false, nil, nil, nil
		}
	}
	var pr *gits.GitPullRequest
	var err error
	var resolver *users.GitUserResolver
	pr, resolver, err = o.getPullRequest(activity)
	if err != nil {
		return false, nil, nil, errors.WithStack(err)
	}
	if len(ignoreLabels) > 0 {

		found := make([]string, 0)
		for _, l := range ignoreLabels {
			for _, v := range pr.Labels {
				if *v.Name == l {
					found = append(found, *v.Name)
				}
			}
		}
		if len(found) > 0 {
			log.Logger().Infof("Ignoring %s because it has labels %s\n", activity.Name, found)
			return false, nil, nil, nil
		}
	}
	return true, pr, resolver, nil
}

func (o *SlackBotOptions) PipelineMessage(activity *record.ActivityRecord) error {

	if activity.Name == "" {
		return fmt.Errorf("PipelineActivity name cannot be empty")
	}

	for _, cfg := range o.Pipelines {
		if enabled, pullRequest, resolver, err := o.isEnabled(activity, cfg.Orgs, cfg.IgnoreLabels); err != nil {
			return errors.WithStack(err)
		} else if enabled {
			attachments, createIfMissing, err := o.createPipelineMessage(activity, pullRequest)
			if err != nil {
				return err
			}
			if cfg.Channel != "" {
				channel := channelName(cfg.Channel)
				err := o.postMessage(channel, false, pipelineMessageType, activity, nil, attachments, createIfMissing)
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("error posting cfg for %s to channel %s", activity.Name,
						channel))
				}
				log.Logger().Infof("Channel message sent to %s\n", cfg.Channel)
			}
			if cfg.DirectMessage {
				if pullRequest != nil {
					id, err := o.resolveGitUserToSlackUser(pullRequest.Author, resolver)
					if err != nil {
						return errors.Wrapf(err, "Cannot resolve Slack ID for Git user %s", pullRequest.Author)
					}
					if id != "" {
						err = o.postMessage(id, true, pipelineMessageType, activity, nil, attachments, createIfMissing)
						if err != nil {
							return errors.Wrap(err, fmt.Sprintf("error sending direct pipeline for %s to %s", activity.Name,
								id))
						}
						log.Logger().Infof("Direct message sent to %s\n", pullRequest.Author)
					}
				}
			}

		}
	}
	return nil
}

func (o *SlackBotOptions) ReviewRequestMessage(activity *record.ActivityRecord) error {

	if activity.Name == "" {
		return fmt.Errorf("PipelineActivity name cannot be empty")
	}

	prn, err := getPullRequestNumber(activity)
	if err != nil {
		return errors.Wrapf(err, "getting pull request number %s", activity.Name)
	}
	if prn > 0 {
		for _, cfg := range o.PullRequests {
			if enabled, pullRequest, resolver, err := o.isEnabled(activity, cfg.Orgs, cfg.IgnoreLabels); err != nil {
				return errors.WithStack(err)
			} else if enabled {
				log.Logger().Infof("Preparing review request message for %s\n", activity.Name)
				oldestActivity, latestActivity, all, err := o.findPipelineActivities(activity)
				if err != nil {
					return err
				}
				buildNumber, err := strconv.Atoi(createPipelineDetails(activity).Build)
				if err != nil {
					return err
				}
				latestBuildNumber := -1
				if latestActivity != nil {
					// TODO Some activities could be missing the labels that identify them properly,
					// in that case just display what we have?
					latestBuildNumber, err = strconv.Atoi(createPipelineDetails(latestActivity).Build)
				}
				if oldestActivity == nil {
					// TODO Some activities could be missing the labels that identify them so what do we do?
					// We at least try to not error
					oldestActivity = activity
				}
				if buildNumber >= latestBuildNumber {
					attachments, reviewers, buildStatus, err := o.createReviewersMessage(activity, cfg.NotifyReviewers,
						pullRequest, resolver)
					if err != nil {
						return err
					}
					createIfMissing := true
					if buildStatus == defaultStatuses.Merged || buildStatus == defaultStatuses.Closed {
						createIfMissing = false
					}
					if attachments != nil {
						if cfg.Channel != "" {
							channel := channelName(cfg.Channel)
							err := o.postMessage(channel, false, pullRequestReviewMessageType, oldestActivity,
								all, attachments, createIfMissing)
							if err != nil {
								return errors.Wrap(err, fmt.Sprintf("error posting PR review request for %s to channel %s",
									activity.Name,
									channel))
							}
						}
						if cfg.DirectMessage && cfg.NotifyReviewers {
							for _, user := range reviewers {
								if user != nil {
									err = o.postMessage(user.ID, true, pullRequestReviewMessageType, oldestActivity,
										all, attachments, createIfMissing)
									if err != nil {
										return errors.Wrap(err, fmt.Sprintf("error sending direct PR review request for %s to %s",
											activity.Name,
											user.ID))
									}
								}
							}

						}
					}
				} else {
					log.Logger().Infof("Skipping %v as it is older than latest build number %d\n", activity.Name,
						latestBuildNumber)
				}
			}
		}
	}
	return nil
}

func (o *SlackBotOptions) isLgtmRepo(activity *record.ActivityRecord) (bool, error) {
	options := prow.Options{
		KubeClient: o.KubeClient,
		NS:         o.Namespace,
	}
	cfg, _, err := options.GetProwConfig()
	if err != nil {
		return false, errors.Wrapf(err, "getting prow config")
	}
	pipeDetails := createPipelineDetails(activity)
	for _, query := range cfg.Keeper.Queries {
		if query.ForRepo(pipeDetails.GitOwner, pipeDetails.GitRepository) {
			if util.Contains(query.Labels, "lgtm") {
				return true, nil
			}
		}
	}
	return false, nil
}

func (o *SlackBotOptions) findPipelineActivities(activity *record.ActivityRecord) (oldest *record.ActivityRecord, latest *record.ActivityRecord, all []*record.ActivityRecord, err error) {
	// This is the trigger activity. Working out the correct slack message is a bit tricky,
	// as we have a 1:n mapping between PRs and PipelineActivities (which store the message info).
	// The algorithm in use just picks the earliest pipeline activity as determined by build number
	prn, err := getPullRequestNumber(activity)
	if err != nil {
		return nil, nil, nil, err
	}

	pipelineDetails := createPipelineDetails(activity)

	acts, err := o.getPipelineActivities(pipelineDetails.GitOwner, pipelineDetails.GitRepository, prn)

	if err != nil {
		return nil, nil, nil, err
	}
	sort.Sort(byBuildNumber(acts.Items))
	var records []*record.ActivityRecord
	for _, a := range acts.Items {
		rec, err := jx.ConvertPipelineActivity(&a)
		if err != nil {
			return nil, nil, nil, err
		}
		records = append(records, rec)
	}

	if len(records) > 0 {
		return records[0], records[len(records)-1], records, nil
	} else {
		log.Logger().Warnf("No pipeline activities exist for %s/%s/pr-%d", pipelineDetails.GitOwner, pipelineDetails.GitRepository, prn)
	}
	return nil, nil, nil, nil
}

func getStatus(overrideStatus *slackapp.Status, defaultStatus *slackapp.Status) *slackapp.Status {
	if overrideStatus == nil {
		return defaultStatus
	}
	return overrideStatus
}

// createReviewersMessage will return a slackapp message notifying reviewers of a PR, or nil if the activity is not a PR
func (o *SlackBotOptions) createReviewersMessage(activity *record.ActivityRecord, notifyReviewers bool, pr *gits.GitPullRequest, resolver *users.GitUserResolver) ([]slack.Attachment, []*slack.User, *slackapp.Status, error) {
	author, err := resolver.Resolve(pr.Author)
	if err != nil {
		return nil, nil, nil, errors.WithStack(err)
	}
	if pr != nil {
		attachments := []slack.Attachment{}
		actions := []slack.AttachmentAction{}
		fallback := []string{}
		status := pipelineStatus(activity)

		authorName, err := o.mentionOrLinkUser(author)
		if err != nil {
			return nil, nil, nil, err
		}

		mentions := make([]string, 0)
		reviewers := make([]*slack.User, 0)
		if notifyReviewers {

			// Match requested requested reviewers to slack users (if possible)
			for _, r := range pr.RequestedReviewers {
				u, err := resolver.Resolve(r)
				if err != nil {
					return nil, nil, nil, errors.Wrapf(err, "resolving %s user %s as Jenkins X user",
						resolver.GitProviderKey(), r.Login)
				}
				if u != nil {
					mention, err := o.mentionOrLinkUser(u)
					if err != nil {
						return nil, nil, nil, errors.Wrapf(err,
							"generating mention or link for user record %s with email %s", u.Name, u.Spec.Email)
					}
					mentions = append(mentions, mention)
				}
			}
		}

		// The default state is not approved
		reviewStatus := getStatus(o.Statuses.NotApproved, defaultStatuses.NotApproved)

		// A bit of a hacky way to do this,
		// but until we get a better CRD based interface to the prow this will work
		lgtmRepo, err := o.isLgtmRepo(activity)
		if err != nil {
			return nil, nil, nil, errors.Wrapf(err, "checking if repo for %s is configured for lgtm", activity.Name)
		}
		if lgtmRepo {
			if containsOneOf(pr.Labels, "lgtm") {
				reviewStatus = getStatus(o.Statuses.LGTM, defaultStatuses.LGTM)
			}
		} else {
			if containsOneOf(pr.Labels, "approved") {
				reviewStatus = getStatus(o.Statuses.Approved, defaultStatuses.Approved)
			}
		}
		if containsOneOf(pr.Labels, "do-not-merge/hold") {
			reviewStatus = getStatus(o.Statuses.Hold, defaultStatuses.Hold)
		}
		if containsOneOf(pr.Labels, "needs-ok-to-test") {
			reviewStatus = getStatus(o.Statuses.NeedsOkToTest, defaultStatuses.NeedsOkToTest)
		}

		// The default build state is unknown
		buildStatus := getStatus(o.Statuses.Unknown, defaultStatuses.Unknown)
		if pr.Merged != nil && *pr.Merged {
			buildStatus = getStatus(o.Statuses.Merged, defaultStatuses.Merged)
		} else if pr.IsClosed() {
			buildStatus = getStatus(o.Statuses.Closed, defaultStatuses.Closed)
		} else {
			switch activity.Status {
			case v1alpha1.PendingState:
				buildStatus = getStatus(o.Statuses.Pending, defaultStatuses.Pending)
			case v1alpha1.RunningState:
				buildStatus = getStatus(o.Statuses.Running, defaultStatuses.Running)
			case v1alpha1.SuccessState:
				buildStatus = getStatus(o.Statuses.Succeeded, defaultStatuses.Succeeded)
			case v1alpha1.FailureState:
				buildStatus = getStatus(o.Statuses.Failed, defaultStatuses.Failed)
			case v1alpha1.AbortedState:
				buildStatus = getStatus(o.Statuses.Aborted, defaultStatuses.Aborted)
			}
		}

		mentionsString := strings.Join(mentions, " ")
		pleaseText := "please"
		if len(mentions) == 0 {
			pleaseText = "Please"
		}
		messageText := fmt.Sprintf("%s %s review %s created on %s by %s",
			mentionsString,
			pleaseText,
			link(fmt.Sprintf("Pull Request %s (%s)", pullRequestName(pr.URL), pr.Title), pr.URL),
			repositoryName(activity),
			authorName)
		attachment := slack.Attachment{
			CallbackID: "preview:" + activity.Name,
			Color:      attachmentColor(status),
			Text:       messageText,

			Fallback: strings.Join(fallback, ", "),
			Actions:  actions,
			Fields: []slack.AttachmentField{
				slack.AttachmentField{
					Value: fmt.Sprintf("%s %s", reviewStatus.Emoji, reviewStatus.Text),
					Short: true,
				},
				slack.AttachmentField{
					Value: fmt.Sprintf("%s %s", buildStatus.Emoji, buildStatus.Text),
					Short: true,
				},
			},
		}
		updatedEpochTime := getLastUpdatedTime(pr, activity)
		if updatedEpochTime > 0 {
			attachment.Ts = json.Number(strconv.FormatInt(updatedEpochTime, 10))
		}

		attachments = append(attachments, attachment)

		return attachments, reviewers, buildStatus, nil
	}
	return nil, nil, nil, nil
}

func getLastUpdatedTime(pr *gits.GitPullRequest, activity *record.ActivityRecord) int64 {
	updatedEpochTime := int64(-1)
	if pr != nil && pr.UpdatedAt != nil {
		updatedEpochTime = pr.UpdatedAt.Unix()
	}
	// Check if there is a started or completion timestamp that is more recent
	if activity != nil && activity.StartTime != nil {
		if start := activity.StartTime.Unix(); start > updatedEpochTime {
			updatedEpochTime = start
		}
	}
	if activity != nil && activity.CompletionTime != nil {
		if completed := activity.CompletionTime.Unix(); completed > updatedEpochTime {
			updatedEpochTime = completed
		}
	}
	return updatedEpochTime
}

func containsOneOf(a []*gits.Label, x ...string) bool {
	for _, n := range a {
		for _, y := range x {
			if y == *n.Name {
				return true
			}
		}
	}
	return false
}

func (o *SlackBotOptions) createPipelineMessage(activity *record.ActivityRecord, pr *gits.GitPullRequest) ([]slack.Attachment, bool, error) {
	status := pipelineStatus(activity)
	icon := pipelineIcon(status)
	pipelineName, err := pipelineName(activity)
	if err != nil {
		return nil, false, errors.Wrapf(err, "getting pipeline name for %s", activity.Name)
	}
	messageText := icon + pipelineName + " " + repositoryName(activity)
	if prn, err := getPullRequestNumber(activity); err != nil {
		return nil, false, err
	} else if prn > 0 {
		messageText = fmt.Sprintf("%s%s", messageText, link(pullRequestName(pr.URL), pr.URL))
	}
	messageText = fmt.Sprintf("%s (Build %s)", messageText, buildNumber(activity))

	attachments := []slack.Attachment{}
	actions := []slack.AttachmentAction{}
	fallback := []string{}
	if activity.GitURL != "" {
		fallback = append(fallback, "Repo: "+activity.GitURL)
		actions = append(actions, slack.AttachmentAction{
			Type: "button",
			Text: "Repository",
			URL:  activity.GitURL,
		})
	}
	if activity.LinkURL != "" {
		fallback = append(fallback, "Build: "+activity.LinkURL)
		actions = append(actions, slack.AttachmentAction{
			Type: "button",
			Text: "Pipeline",
			URL:  activity.LinkURL,
		})
	}
	if activity.LogURL != "" {
		fallback = append(fallback, "Logs: "+activity.LogURL)
		actions = append(actions, slack.AttachmentAction{
			Type: "button",
			Text: "Build Logs",
			URL:  strings.Replace(activity.LogURL, "gs://", "https://storage.cloud.google.com/", -1),
		})
	}
	attachment := slack.Attachment{
		CallbackID: "pipelineactivity:" + activity.Name,
		Color:      attachmentColor(status),
		Title:      messageText,
		Fallback:   strings.Join(fallback, ", "),
		Actions:    actions,
	}

	lastUpdatedTime := getLastUpdatedTime(nil, activity)
	if lastUpdatedTime > 0 {
		attachment.Ts = json.Number(strconv.FormatInt(lastUpdatedTime, 10))
	}
	dayAgo := time.Now().Add(time.Duration((-24) * time.Hour)).Unix()
	createIfMissing := true
	if lastUpdatedTime < dayAgo {
		createIfMissing = false
	}

	attachments = append(attachments, attachment)

	for _, step := range activity.Stages {
		stepAttachments := o.createAttachments(activity, step)
		if len(stepAttachments) > 0 {
			attachments = append(attachments, stepAttachments...)
		}
	}

	return attachments, createIfMissing, nil
}

func (o *SlackBotOptions) getSlackUserID(gitUser *gits.GitUser, resolver *users.GitUserResolver) (string, error) {
	if gitUser == nil {
		return "", fmt.Errorf("User cannot be nil")
	}
	user, err := resolver.Resolve(gitUser)
	if err != nil {
		return "", err
	}

	return o.SlackUserResolver.SlackUserLogin(user)
}

// getPullRequestNumber extracts the pull request number from the activity or returns 0 if it's not a pull request
func getPullRequestNumber(activity *record.ActivityRecord) (int, error) {
	pipelineDetails := createPipelineDetails(activity)
	if strings.HasPrefix(strings.ToLower(pipelineDetails.BranchName), "pr-") {
		return strconv.Atoi(strings.TrimPrefix(strings.ToLower(pipelineDetails.BranchName), "pr-"))
	}
	return 0, nil
}

func (o *SlackBotOptions) postMessage(channel string, directMessage bool, messageType string,
	activity *record.ActivityRecord, all []*record.ActivityRecord, attachments []slack.Attachment,
	createIfMissing bool) error {
	timestamp := ""
	channelId := channel

	messageRef := o.Timestamps[channel][activity.Name]

	if messageRef != nil {
		timestamp = messageRef.Timestamp
		channelId = messageRef.ChannelID
	}

	if _, ok := o.Timestamps[channel]; !ok {
		o.Timestamps[channel] = make(map[string]*MessageReference, 0)
	}
	//channelID, timestamp, err := o.SlackClient.PostMessage(o.Channels, messageText, params, slackbot.MsgOptionUpdate(timestamp))
	options := []slack.MsgOption{
		slack.MsgOptionAttachments(attachments...),
	}
	if directMessage {
		channel, _, _, err := o.SlackClient.OpenConversation(&slack.OpenConversationParameters{
			Users: []string{
				channel,
			},
		})
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("(open converation channelId: %s)", channelId))
		}
		channelId = channel.ID
	}
	post := true
	if timestamp != "" {
		options = append(options, slack.MsgOptionUpdate(timestamp))
		log.Logger().Infof("Updating message for %s with timestamp %s\n", activity.Name, timestamp)
	} else {
		if createIfMissing {
			log.Logger().Infof("Creating new message for %s\n", activity.Name)
		} else {
			log.Logger().Infof("No existing message to update, ignoring, for %s\n", activity.Name)
			post = false
		}

	}
	if post {
		channelId, timestamp, _, err := o.SlackClient.SendMessageContext(context.Background(), channelId, options...)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("(post channelId: %s, timestamp: %s)", channelId, timestamp))
		}
		o.Timestamps[channel][activity.Name] = &MessageReference{
			ChannelID: channelId,
			Timestamp: timestamp,
		}
	}
	return nil
}

//getPullRequest will return the PullRequestInfo for the activity, or nil if it's not a pull request
func (o *SlackBotOptions) getPullRequest(activity *record.ActivityRecord) (pr *gits.GitPullRequest,
	resolver *users.GitUserResolver, err error) {
	if prn, err := getPullRequestNumber(activity); prn > 0 {
		if err != nil {
			return nil, nil, err
		}
		if activity.GitURL == "" {
			return nil, nil, fmt.Errorf("no GitURL on PipelineActivity %s", activity.Name)
		}
		gitProvider, gitInfo, err := o.CommonOptions.CreateGitProviderForURLWithoutKind(activity.GitURL)
		if err != nil {
			return nil, nil, err
		}
		prn, err := getPullRequestNumber(activity)
		if err != nil {
			return nil, nil, err
		}
		resolver := &users.GitUserResolver{
			Namespace:   o.Namespace,
			GitProvider: gitProvider,
			JXClient:    o.JXClient,
		}
		pr, err := gitProvider.GetPullRequest(gitInfo.Organisation, gitInfo, prn)
		return pr, resolver, err
	}
	return nil, nil, nil
}

func annotationKey(channel string, messageType string) string {
	return fmt.Sprintf("%s-%s/%s", SlackAnnotationPrefix, messageType, strings.TrimPrefix(channel, "#"))
}

func (o *SlackBotOptions) createAttachments(activity *record.ActivityRecord,
	step *record.ActivityStageOrStep) []slack.Attachment {
	if step != nil {
		return o.createStageAttachments(activity, step)
	}
	return []slack.Attachment{}

}

func (o *SlackBotOptions) createStageAttachments(activity *record.ActivityRecord,
	stage *record.ActivityStageOrStep) []slack.Attachment {
	name := stage.Name
	if name == "" {
		name = "Stage"
	}
	attachments := []slack.Attachment{
		o.createStepAttachment(stage, name, "", ""),
	}
	if stage.Name != "meta pipeline" {
		for _, step := range stage.Steps {
			// filter out tekton generated steps
			if isUserPipelineStep(step.Name) {
				attachments = append(attachments, o.createStepAttachment(step, "", "", ""))
			}
		}
	}

	return attachments
}

func isUserPipelineStep(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	ss := strings.Fields(name)
	firstWord := ss[0]

	if containsIgnoreCase(knownPipelineStageTypes, firstWord) {
		return true
	}
	return false
}

func (o *SlackBotOptions) createStepAttachment(step *record.ActivityStageOrStep, name string, description string,
	iconUrl string) slack.Attachment {
	text := description

	textName := strings.Title(name)
	if textName == "" {
		textName = step.Name
	}

	textName = getUserFriendlyMapping(textName)

	stepStatus := step.Status
	textMessage := o.statusString(stepStatus) + " " + textName
	if text != "" {
		textMessage += " " + text
	}

	return slack.Attachment{
		Text:       textMessage,
		FooterIcon: iconUrl,
		MarkdownIn: []string{"fields"},
		Color:      attachmentColor(stepStatus),
	}
}

func (o *SlackBotOptions) annotatePipelineActivity(activity *jenkinsv1.PipelineActivity, key string, value string) error {
	newActivity := activity.DeepCopy()
	if newActivity.Annotations == nil {
		newActivity.Annotations = make(map[string]string)
	}
	newActivity.Annotations[key] = value
	patch, err := CreatePatch(activity, newActivity)
	if err != nil {
		return errors.Wrapf(err, "creating patch to add annotation %s=%s to %s", key, value, activity.Name)
	}
	jsonPatch, err := json.Marshal(patch)
	if err != nil {
		return errors.Wrapf(err, "marshaling patch to add annotation %s=%s to %s", key, value, activity.Name)
	}
	_, err = o.JXClient.JenkinsV1().PipelineActivities(o.Namespace).Patch(activity.Name, types.JSONPatchType,
		jsonPatch)
	return err
}

func pullRequestName(url string) string {
	idx := strings.LastIndex(url, "/")
	if idx > 0 {
		return "#" + url[idx+1:]
	}
	return url
}

func pipelineName(activity *record.ActivityRecord) (string, error) {
	name := fmt.Sprintf("%s/%s/%s", activity.Owner, activity.Repo, activity.Branch)
	if strings.HasSuffix(name, "/master") {
		return "Release Pipeline", nil
	}
	prn, err := getPullRequestNumber(activity)
	if err != nil {
		return "", errors.Wrapf(err, "getting pull request number from %s", activity.Name)
	}
	if prn > 0 {
		return "Pull Request Pipeline", nil
	}
	return "Pipeline", nil
}

func repositoryName(act *record.ActivityRecord) string {
	details := createPipelineDetails(act)
	gitURL := act.GitURL
	ownerURL := strings.TrimSuffix(gitURL, "/")
	idx := strings.LastIndex(ownerURL, "/")
	if idx > 0 {
		ownerURL = ownerURL[0 : idx+1]
	}
	return link(details.GitOwner, ownerURL) + "/" + link(details.GitRepository, gitURL)
}

func (o *SlackBotOptions) mentionOrLinkUser(user *jenkinsv1.User) (string, error) {
	id, err := o.SlackUserResolver.SlackUserLogin(user)
	if err != nil {
		return "", err
	}
	if id != "" {
		return mentionUser(id), nil
	}
	if user.Spec.Name != "" && user.Spec.URL != "" {
		return link(user.Spec.Name, user.Spec.URL), nil
	}
	if user.Spec.Name != "" {
		return user.Spec.Name, nil
	}
	return "", nil
}

func buildNumber(activity *record.ActivityRecord) string {
	return link("#"+activity.BuildIdentifier, activity.LinkURL)
}

func channelName(channel string) string {
	if !strings.HasPrefix(channel, "#") {
		return fmt.Sprintf("#%s", channel)
	}
	return channel
}

func link(text string, url string) string {
	if url != "" {
		if text == "" {
			text = url
		}
		return "<" + url + "|" + text + ">"
	} else {
		return text
	}
}

func mergeShaText(gitURL, sha string) string {
	short := sha[0:7]
	cleanUrl := strings.TrimSuffix(gitURL, ".git")
	if cleanUrl != "" {
		cleanUrl = util.UrlJoin(cleanUrl, "commit", sha)
	}
	return link(short, cleanUrl)
}

func pullRequestStatusString(text string) string {
	title := strings.Title(text)
	switch text {
	case "success":
		return title
	case "error", "failed":
		return title
	default:
		return title
	}
}

func (o *SlackBotOptions) resolveGitUserToSlackUser(user *gits.GitUser, resolver *users.GitUserResolver) (string,
	error) {
	resolved, err := resolver.Resolve(user)
	if err != nil {
		return "", err
	}
	return o.SlackUserResolver.SlackUserLogin(resolved)
}

func (o *SlackBotOptions) statusString(statusType v1alpha1.PipelineState) string {
	switch statusType {
	case v1alpha1.FailureState, v1alpha1.AbortedState:
		return getStatus(o.Statuses.Failed, defaultStatuses.Failed).Emoji
	case v1alpha1.SuccessState:
		return getStatus(o.Statuses.Succeeded, defaultStatuses.Succeeded).Emoji
	case v1alpha1.RunningState, v1alpha1.PendingState:
		return getStatus(o.Statuses.Running, defaultStatuses.Running).Emoji
	}
	return ""
}

func attachmentColor(statusType v1alpha1.PipelineState) string {
	switch statusType {
	case v1alpha1.FailureState:
		return "danger"
	case v1alpha1.SuccessState:
		return "good"
	case v1alpha1.RunningState, v1alpha1.PendingState:
		return "#3AA3E3"
	}
	return ""
}

func pullRequestIcon(step *jenkinsv1.PromotePullRequestStep) string {
	state := "open"
	switch step.Status {
	case jenkinsv1.ActivityStatusTypeFailed, jenkinsv1.ActivityStatusTypeError:
		state = "closed"
	case jenkinsv1.ActivityStatusTypeSucceeded:
		state = "merged"
	}
	return "https://images.atomist.com/rug/pull-request-" + state + ".png"
}

func pipelineStatus(activity *record.ActivityRecord) v1alpha1.PipelineState {
	statusType := activity.Status
	switch statusType {
	case v1alpha1.AbortedState, v1alpha1.FailureState:
	case v1alpha1.SuccessState:
		return statusType
	}
	// lets try find the last status
	for _, step := range activity.Stages {
		statusType = step.Status
	}
	return statusType
}

func pipelineIcon(statusType v1alpha1.PipelineState) string {
	switch statusType {
	case v1alpha1.FailureState:
		return ""
	case v1alpha1.SuccessState:
		return ""
	case v1alpha1.RunningState, v1alpha1.PendingState:
		return ""
	}
	return ""
}

func mentionUser(id string) string {
	return fmt.Sprintf("<@%s>", id)
}

// createPipelineDetails creates a PipelineDetails object populated from the activity
func createPipelineDetails(activity *record.ActivityRecord) *kube.PipelineDetails {
	repoOwner := activity.Owner
	repoName := activity.Repo
	buildNumber := activity.BuildIdentifier
	branchName := ""
	context := activity.Context
	pipeline := fmt.Sprintf("%s/%s/%s", activity.Owner, activity.Repo, activity.Branch)
	if pipeline != "" {
		paths := strings.Split(pipeline, "/")
		if len(paths) > 2 {
			if repoOwner == "" {
				repoOwner = paths[0]
			}
			if repoName == "" {
				repoName = paths[1]
			}
			branchName = paths[2]
		}
	}
	if branchName == "" {
		branchName = "master"
	}
	if pipeline == "" && (repoName != "" && repoOwner != "") {
		pipeline = repoOwner + "/" + repoName + "/" + branchName
	}
	return &kube.PipelineDetails{
		GitOwner:      repoOwner,
		GitRepository: repoName,
		BranchName:    branchName,
		Pipeline:      pipeline,
		Build:         buildNumber,
		Context:       context,
	}
}
