package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/github/gh-cli/api"
	"github.com/github/gh-cli/context"
	"github.com/github/gh-cli/git"
	"github.com/github/gh-cli/utils"
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(prCmd)
	prCmd.AddCommand(prCheckoutCmd)
	prCmd.AddCommand(prCreateCmd)
	prCmd.AddCommand(prListCmd)
	prCmd.AddCommand(prStatusCmd)
	prCmd.AddCommand(prViewCmd)

	prListCmd.Flags().IntP("limit", "L", 30, "Maximum number of items to fetch")
	prListCmd.Flags().StringP("state", "s", "open", "Filter by state")
	prListCmd.Flags().StringP("base", "B", "", "Filter by base branch")
	prListCmd.Flags().StringSliceP("label", "l", nil, "Filter by label")
	prListCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
}

var prCmd = &cobra.Command{
	Use:   "pr",
	Short: "Create, view, and checkout pull requests",
	Long:  `Work with GitHub pull requests.`,
}
var prCheckoutCmd = &cobra.Command{
	Use:   "checkout <pr-number>",
	Short: "Check out a pull request in Git",
	Args:  cobra.MinimumNArgs(1),
	RunE:  prCheckout,
}
var prListCmd = &cobra.Command{
	Use:   "list",
	Short: "List and filter pull requests in this repository",
	RunE:  prList,
}
var prStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of relevant pull requests",
	RunE:  prStatus,
}
var prViewCmd = &cobra.Command{
	Use:   "view [pr-number]",
	Short: "View a pull request in the browser",
	RunE:  prView,
}

func prStatus(cmd *cobra.Command, args []string) error {
	ctx := contextForCommand(cmd)
	apiClient, err := apiClientForContext(ctx)
	if err != nil {
		return err
	}

	baseRepo, err := ctx.BaseRepo()
	if err != nil {
		return err
	}
	currentPRNumber, currentPRHeadRef, err := prSelectorForCurrentBranch(ctx)
	if err != nil {
		return err
	}
	currentUser, err := ctx.AuthLogin()
	if err != nil {
		return err
	}

	prPayload, err := api.PullRequests(apiClient, baseRepo, currentPRNumber, currentPRHeadRef, currentUser)
	if err != nil {
		return err
	}

	out := colorableOut(cmd)

	printHeader(out, "Current branch")
	if prPayload.CurrentPR != nil {
		printPrs(out, *prPayload.CurrentPR)
	} else {
		message := fmt.Sprintf("  There is no pull request associated with %s", utils.Cyan("["+currentPRHeadRef+"]"))
		printMessage(out, message)
	}
	fmt.Fprintln(out)

	printHeader(out, "Created by you")
	if len(prPayload.ViewerCreated) > 0 {
		printPrs(out, prPayload.ViewerCreated...)
	} else {
		printMessage(out, "  You have no open pull requests")
	}
	fmt.Fprintln(out)

	printHeader(out, "Requesting a code review from you")
	if len(prPayload.ReviewRequested) > 0 {
		printPrs(out, prPayload.ReviewRequested...)
	} else {
		printMessage(out, "  You have no pull requests to review")
	}
	fmt.Fprintln(out)

	return nil
}

func prList(cmd *cobra.Command, args []string) error {
	ctx := contextForCommand(cmd)
	apiClient, err := apiClientForContext(ctx)
	if err != nil {
		return err
	}

	baseRepo, err := ctx.BaseRepo()
	if err != nil {
		return err
	}

	limit, err := cmd.Flags().GetInt("limit")
	if err != nil {
		return err
	}
	state, err := cmd.Flags().GetString("state")
	if err != nil {
		return err
	}
	baseBranch, err := cmd.Flags().GetString("base")
	if err != nil {
		return err
	}
	labels, err := cmd.Flags().GetStringSlice("label")
	if err != nil {
		return err
	}
	assignee, err := cmd.Flags().GetString("assignee")
	if err != nil {
		return err
	}

	var graphqlState []string
	switch state {
	case "open":
		graphqlState = []string{"OPEN"}
	case "closed":
		graphqlState = []string{"CLOSED"}
	case "merged":
		graphqlState = []string{"MERGED"}
	case "all":
		graphqlState = []string{"OPEN", "CLOSED", "MERGED"}
	default:
		return fmt.Errorf("invalid state: %s", state)
	}

	params := map[string]interface{}{
		"owner": baseRepo.RepoOwner(),
		"repo":  baseRepo.RepoName(),
		"state": graphqlState,
	}
	if len(labels) > 0 {
		params["labels"] = labels
	}
	if baseBranch != "" {
		params["baseBranch"] = baseBranch
	}
	if assignee != "" {
		params["assignee"] = assignee
	}

	prs, err := api.PullRequestList(apiClient, params, limit)
	if err != nil {
		return err
	}

	table := utils.NewTablePrinter(cmd.OutOrStdout())
	for _, pr := range prs {
		prNum := strconv.Itoa(pr.Number)
		if table.IsTTY() {
			prNum = "#" + prNum
		}
		table.AddField(prNum, nil, colorFuncForState(pr.State))
		table.AddField(pr.Title, nil, nil)
		table.AddField(pr.HeadLabel(), nil, utils.Cyan)
		table.EndRow()
	}
	err = table.Render()
	if err != nil {
		return err
	}

	return nil
}

func colorFuncForState(state string) func(string) string {
	switch state {
	case "OPEN":
		return utils.Green
	case "CLOSED":
		return utils.Red
	case "MERGED":
		return utils.Magenta
	default:
		return nil
	}
}

func prView(cmd *cobra.Command, args []string) error {
	ctx := contextForCommand(cmd)
	baseRepo, err := ctx.BaseRepo()
	if err != nil {
		return err
	}

	var openURL string
	if len(args) > 0 {
		if prNumber, err := strconv.Atoi(args[0]); err == nil {
			// TODO: move URL generation into GitHubRepository
			openURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", baseRepo.RepoOwner(), baseRepo.RepoName(), prNumber)
		} else {
			return fmt.Errorf("invalid pull request number: '%s'", args[0])
		}
	} else {
		prNumber, branchWithOwner, err := prSelectorForCurrentBranch(ctx)
		if err != nil {
			return err
		}

		if prNumber > 0 {
			openURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", baseRepo.RepoOwner(), baseRepo.RepoName(), prNumber)
		} else {
			apiClient, err := apiClientForContext(ctx)
			if err != nil {
				return err
			}

			pr, err := api.PullRequestForBranch(apiClient, baseRepo, branchWithOwner)
			if err != nil {
				return err
			}
			openURL = pr.URL
		}
	}

	cmd.Printf("Opening %s in your browser.\n", openURL)
	return utils.OpenInBrowser(openURL)
}

func prSelectorForCurrentBranch(ctx context.Context) (prNumber int, prHeadRef string, err error) {
	baseRepo, err := ctx.BaseRepo()
	if err != nil {
		return
	}
	prHeadRef, err = ctx.Branch()
	if err != nil {
		return
	}
	branchConfig := git.ReadBranchConfig(prHeadRef)

	// the branch is configured to merge a special PR head ref
	prHeadRE := regexp.MustCompile(`^refs/pull/(\d+)/head$`)
	if m := prHeadRE.FindStringSubmatch(branchConfig.MergeRef); m != nil {
		prNumber, _ = strconv.Atoi(m[1])
		return
	}

	var branchOwner string
	if branchConfig.RemoteURL != nil {
		// the branch merges from a remote specified by URL
		if r, err := context.RepoFromURL(branchConfig.RemoteURL); err == nil {
			branchOwner = r.RepoOwner()
		}
	} else if branchConfig.RemoteName != "" {
		// the branch merges from a remote specified by name
		rem, _ := ctx.Remotes()
		if r, err := rem.FindByName(branchConfig.RemoteName); err == nil {
			branchOwner = r.RepoOwner()
		}
	}

	if branchOwner != "" {
		if strings.HasPrefix(branchConfig.MergeRef, "refs/heads/") {
			prHeadRef = strings.TrimPrefix(branchConfig.MergeRef, "refs/heads/")
		}
		// prepend `OWNER:` if this branch is pushed to a fork
		if !strings.EqualFold(branchOwner, baseRepo.RepoOwner()) {
			prHeadRef = fmt.Sprintf("%s:%s", branchOwner, prHeadRef)
		}
	}

	return
}

func prCheckout(cmd *cobra.Command, args []string) error {
	prNumber, err := strconv.Atoi(args[0])
	if err != nil {
		return err
	}

	ctx := contextForCommand(cmd)
	currentBranch, _ := ctx.Branch()
	remotes, err := ctx.Remotes()
	if err != nil {
		return err
	}
	// FIXME: duplicates logic from fsContext.BaseRepo
	baseRemote, err := remotes.FindByName("upstream", "github", "origin", "*")
	if err != nil {
		return err
	}
	apiClient, err := apiClientForContext(ctx)
	if err != nil {
		return err
	}

	pr, err := api.PullRequestByNumber(apiClient, baseRemote, prNumber)
	if err != nil {
		return err
	}

	headRemote := baseRemote
	if pr.IsCrossRepository {
		headRemote, _ = remotes.FindByRepo(pr.HeadRepositoryOwner.Login, pr.HeadRepository.Name)
	}

	cmdQueue := [][]string{}

	newBranchName := pr.HeadRefName
	if headRemote != nil {
		// there is an existing git remote for PR head
		remoteBranch := fmt.Sprintf("%s/%s", headRemote.Name, pr.HeadRefName)
		refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s", pr.HeadRefName, remoteBranch)

		cmdQueue = append(cmdQueue, []string{"git", "fetch", headRemote.Name, refSpec})

		// local branch already exists
		if git.VerifyRef("refs/heads/" + newBranchName) {
			cmdQueue = append(cmdQueue, []string{"git", "checkout", newBranchName})
			cmdQueue = append(cmdQueue, []string{"git", "merge", "--ff-only", fmt.Sprintf("refs/remotes/%s", remoteBranch)})
		} else {
			cmdQueue = append(cmdQueue, []string{"git", "checkout", "-b", newBranchName, "--no-track", remoteBranch})
			cmdQueue = append(cmdQueue, []string{"git", "config", fmt.Sprintf("branch.%s.remote", newBranchName), headRemote.Name})
			cmdQueue = append(cmdQueue, []string{"git", "config", fmt.Sprintf("branch.%s.merge", newBranchName), "refs/heads/" + pr.HeadRefName})
		}
	} else {
		// no git remote for PR head

		// avoid naming the new branch the same as the default branch
		if newBranchName == pr.HeadRepository.DefaultBranchRef.Name {
			newBranchName = fmt.Sprintf("%s/%s", pr.HeadRepositoryOwner.Login, newBranchName)
		}

		ref := fmt.Sprintf("refs/pull/%d/head", prNumber)
		if newBranchName == currentBranch {
			// PR head matches currently checked out branch
			cmdQueue = append(cmdQueue, []string{"git", "fetch", baseRemote.Name, ref})
			cmdQueue = append(cmdQueue, []string{"git", "merge", "--ff-only", "FETCH_HEAD"})
		} else {
			// create a new branch
			cmdQueue = append(cmdQueue, []string{"git", "fetch", baseRemote.Name, fmt.Sprintf("%s:%s", ref, newBranchName)})
			cmdQueue = append(cmdQueue, []string{"git", "checkout", newBranchName})
		}

		remote := baseRemote.Name
		mergeRef := ref
		if pr.MaintainerCanModify {
			remote = fmt.Sprintf("https://github.com/%s/%s.git", pr.HeadRepositoryOwner.Login, pr.HeadRepository.Name)
			mergeRef = fmt.Sprintf("refs/heads/%s", pr.HeadRefName)
		}
		if mc, err := git.Config(fmt.Sprintf("branch.%s.merge", newBranchName)); err != nil || mc == "" {
			cmdQueue = append(cmdQueue, []string{"git", "config", fmt.Sprintf("branch.%s.remote", newBranchName), remote})
			cmdQueue = append(cmdQueue, []string{"git", "config", fmt.Sprintf("branch.%s.merge", newBranchName), mergeRef})
		}
	}

	for _, args := range cmdQueue {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := utils.PrepareCmd(cmd).Run(); err != nil {
			return err
		}
	}

	return nil
}

func printPrs(w io.Writer, prs ...api.PullRequest) {
	for _, pr := range prs {
		prNumber := fmt.Sprintf("#%d", pr.Number)
		fmt.Fprintf(w, "  %s  %s %s", utils.Yellow(prNumber), truncate(50, pr.Title), utils.Cyan("["+pr.HeadLabel()+"]"))

		checks := pr.ChecksStatus()
		reviews := pr.ReviewStatus()
		if checks.Total > 0 || reviews.ChangesRequested || reviews.Approved {
			fmt.Fprintf(w, "\n  ")
		}

		if checks.Total > 0 {
			var summary string
			if checks.Failing > 0 {
				if checks.Failing == checks.Total {
					summary = utils.Red("All checks failing")
				} else {
					summary = utils.Red(fmt.Sprintf("%d/%d checks failing", checks.Failing, checks.Total))
				}
			} else if checks.Pending > 0 {
				summary = utils.Yellow("Checks pending")
			} else if checks.Passing == checks.Total {
				summary = utils.Green("Checks passing")
			}
			fmt.Fprintf(w, " - %s", summary)
		}

		if reviews.ChangesRequested {
			fmt.Fprintf(w, " - %s", utils.Red("changes requested"))
		} else if reviews.ReviewRequired {
			fmt.Fprintf(w, " - %s", utils.Yellow("review required"))
		} else if reviews.Approved {
			fmt.Fprintf(w, " - %s", utils.Green("approved"))
		}

		fmt.Fprint(w, "\n")
	}
}

func printHeader(w io.Writer, s string) {
	fmt.Fprintln(w, utils.Bold(s))
}

func printMessage(w io.Writer, s string) {
	fmt.Fprintln(w, utils.Gray(s))
}

func truncate(maxLength int, title string) string {
	if len(title) > maxLength {
		return title[0:maxLength-3] + "..."
	}
	return title
}