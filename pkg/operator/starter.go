package operator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/eparis/bugzilla"
	"github.com/openshift/library-go/pkg/controller/factory"
	slackgo "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/mfojtik/bugzilla-operator/pkg/cache"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/closecontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/controller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/firstteamcommentcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/newcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/blockers"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/closed"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/incoming"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/upcomingsprint"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/resetcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/stalecontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/slack"
	"github.com/mfojtik/bugzilla-operator/pkg/slacker"
)

const bugzillaEndpoint = "https://bugzilla.redhat.com"

func Run(ctx context.Context, cfg config.OperatorConfig) error {
	if len(cfg.CachePath) > 0 {
		cache.Open(cfg.CachePath)
	}
	defer cache.Close()

	slackClient := slackgo.New(cfg.Credentials.DecodedSlackToken(), slackgo.OptionDebug(true))

	// This slack client is used for debugging
	slackDebugClient := slack.NewChannelClient(slackClient, cfg.SlackAdminChannel, cfg.SlackAdminChannel, true)

	// This slack client posts only to the admin channel
	slackAdminClient := slack.NewChannelClient(slackClient, cfg.SlackAdminChannel, cfg.SlackAdminChannel, false)

	recorder := slack.NewRecorder(slackAdminClient, "BugzillaOperator")

	defer func() {
		recorder.Warningf("Shutdown", ":crossed_fingers: *The bot is shutting down*")
	}()

	slackerInstance := slacker.NewSlacker(slackClient, slacker.Options{
		ListenAddress:     "0.0.0.0:3000",
		VerificationToken: cfg.Credentials.DecodedSlackVerificationToken(),
	})
	slackerInstance.Command("say <message>", &slacker.CommandDefinition{
		Description: "Say something.",
		Handler: func(req slacker.Request, w slacker.ResponseWriter) {
			msg := req.StringParam("message", "")
			w.Reply(msg)
		},
	})
	slackerInstance.DefaultCommand(func(req slacker.Request, w slacker.ResponseWriter) {
		w.Reply("Unknown command")
	})

	recorder.Eventf("OperatorStarted", "Bugzilla Operator Started\n\n```\n%s\n```\n", spew.Sdump(cfg.Anonymize()))

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}
	cmClient := kubeClient.CoreV1().ConfigMaps(os.Getenv("POD_NAMESPACE"))

	controllerContext := controller.NewControllerContext(newBugzillaClient(&cfg, slackDebugClient), slackAdminClient, slackDebugClient, cmClient)
	controllers := map[string]factory.Controller{
		"stale":              stalecontroller.NewStaleController(controllerContext, cfg, recorder),
		"stale-reset":        resetcontroller.NewResetStaleController(controllerContext, cfg, recorder),
		"close-stale":        closecontroller.NewCloseStaleController(controllerContext, cfg, recorder),
		"first-team-comment": firstteamcommentcontroller.NewFirstTeamCommentController(controllerContext, cfg, recorder),
		"new":                newcontroller.NewNewBugController(controllerContext, cfg, recorder),
	}

	// TODO: enable by default
	cfg.DisabledControllers = append(cfg.DisabledControllers, "NewBugController")

	var scheduledReports []factory.Controller
	reportNames := sets.NewString()
	newReport := func(name string, ctx controller.ControllerContext, components, when []string) factory.Controller {
		switch name {
		case "blocker-bugs":
			return blockers.NewBlockersReporter(ctx, components, when, cfg, recorder)
		case "incoming-bugs":
			return incoming.NewIncomingReporter(ctx, when, cfg, recorder)
		case "incoming-stats":
			return incoming.NewIncomingStatsReporter(ctx, when, cfg, recorder)
		case "closed-bugs":
			return closed.NewClosedReporter(ctx, components, when, cfg, recorder)
		case "upcoming-sprint":
			return upcomingsprint.NewUpcomingSprintReporter(controllerContext, components, when, cfg, recorder)
		default:
			return nil
		}
	}
	for _, ar := range cfg.Schedules {
		slackChannelClient := slack.NewChannelClient(slackClient, ar.SlackChannel, cfg.SlackAdminChannel, false)
		reporterContext := controller.NewControllerContext(newBugzillaClient(&cfg, slackDebugClient), slackChannelClient, slackDebugClient, cmClient)
		for _, r := range ar.Reports {
			if c := newReport(r, reporterContext, ar.Components, ar.When); c != nil {
				scheduledReports = append(scheduledReports, c)
				reportNames.Insert(r)
			}
		}
	}
	debugReportControllers := map[string]factory.Controller{}
	for _, r := range reportNames.List() {
		debugReportControllers[r] = newReport(r, controllerContext, cfg.Components.List(), nil)
	}

	controllerNames := sets.NewString()
	for n := range controllers {
		controllerNames.Insert(n)
	}

	// allow to manually trigger a controller to run out of its normal schedule
	runJob := func(debug bool) func(req slacker.Request, w slacker.ResponseWriter) {
		return func(req slacker.Request, w slacker.ResponseWriter) {
			job := req.StringParam("job", "")

			c, ok := controllers[job]
			if !ok {
				if !debug {
					w.Reply(fmt.Sprintf("Unknown job %q", job))
					return
				}
				if c, ok = debugReportControllers[job]; !ok {
					w.Reply(fmt.Sprintf("Unknown job %q", job))
					return
				}
			}

			ctx := ctx // shadow global ctx
			if debug {
				ctx = context.WithValue(ctx, "debug", debug)
			}

			startTime := time.Now()
			_, _, _, err := w.Client().SendMessage(req.Event().Channel,
				slackgo.MsgOptionPostEphemeral(req.Event().User),
				slackgo.MsgOptionText(fmt.Sprintf("Triggering job %q", job), false))
			if err != nil {
				klog.Error(err)
			}
			if err := c.Sync(ctx, factory.NewSyncContext(job, recorder)); err != nil {
				recorder.Warningf("ReportError", "Job reported error: %v", err)
				return
			}
			_, _, _, err = w.Client().SendMessage(req.Event().Channel,
				slackgo.MsgOptionPostEphemeral(req.Event().User),
				slackgo.MsgOptionText(fmt.Sprintf("Finished job %q after %v", job, time.Since(startTime)), false))
			if err != nil {
				klog.Error(err)
			}
		}
	}
	slackerInstance.Command("admin trigger <job>", &slacker.CommandDefinition{
		Description: fmt.Sprintf("Trigger a job to run: %s", strings.Join(controllerNames.List(), ", ")),
		Handler:     auth(cfg, runJob(false), "group:admins"),
	})
	slackerInstance.Command("admin debug <job>", &slacker.CommandDefinition{
		Description: fmt.Sprintf("Trigger a job to run in debug mode: %s", strings.Join(append(controllerNames.List(), reportNames.List()...), ", ")),
		Handler:     auth(cfg, runJob(true), "group:admins"),
	})
	slackerInstance.Command("report <job>", &slacker.CommandDefinition{
		Description: fmt.Sprintf("Run a report and print result here: %s", strings.Join(reportNames.List(), ", ")),
		Handler: func(req slacker.Request, w slacker.ResponseWriter) {
			job := req.StringParam("job", "")
			reports := map[string]func(ctx context.Context, client cache.BugzillaClient) (string, error){
				"blocker-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					report, _, err := blockers.Report(ctx, client, recorder, &cfg, cfg.Components.List())
					return report, err
				},
				"closed-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					return closed.Report(ctx, client, recorder, &cfg, cfg.Components.List())
				},
				"incoming-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					report, _, _, err := incoming.Report(ctx, client, recorder, &cfg)
					return report, err
				},
				"incoming-stats": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					report, err := incoming.ReportStats(ctx, controllerContext, recorder, &cfg)
					return report, err
				},
				"upcoming-sprint": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					return upcomingsprint.Report(ctx, client, recorder, &cfg, cfg.Components.List())
				},

				// don't forget to also add new reports above in the trigger command
			}

			report, ok := reports[job]
			if !ok {
				w.Reply(fmt.Sprintf("Unknown report %q", job))
				return
			}

			_, _, _, err := w.Client().SendMessage(req.Event().Channel,
				slackgo.MsgOptionPostEphemeral(req.Event().User),
				slackgo.MsgOptionText(fmt.Sprintf("Running job %q. This might take some seconds.", job), false))
			if err != nil {
				klog.Error(err)
			}

			reply, err := report(context.TODO(), newBugzillaClient(&cfg, slackDebugClient)(true)) // report should never write anything to BZ
			if err != nil {
				_, _, _, err := w.Client().SendMessage(req.Event().Channel,
					slackgo.MsgOptionPostEphemeral(req.Event().User),
					slackgo.MsgOptionText(fmt.Sprintf("Error running report %v: %v", job, err), false))
				if err != nil {
					klog.Error(err)
				}
			} else {
				w.Reply(reply)
			}
		},
	})

	seen := []string{}
	disabled := sets.NewString(cfg.DisabledControllers...)
	var all []factory.Controller
	for _, c := range controllers {
		all = append(all, c)
	}
	for _, c := range append(all, scheduledReports...) {
		seen = append(seen, c.Name())
		if disabled.Has(c.Name()) {
			continue
		}
		go c.Run(ctx, 1)
	}

	go slackerInstance.Run(ctx)

	// sanity check list of disabled controllers
	unknown := disabled.Difference(sets.NewString(seen...))
	if unknown.Len() > 0 {
		msg := fmt.Sprintf("Unknown disabled controllers in config: %v", unknown.List())
		klog.Warning(msg)
		slackAdminClient.MessageAdminChannel(msg)
	}

	<-ctx.Done()

	return nil
}

func newBugzillaClient(cfg *config.OperatorConfig, slackDebugClient slack.ChannelClient) func(debug bool) cache.BugzillaClient {
	return func(debug bool) cache.BugzillaClient {
		c := cache.NewCachedBugzillaClient(bugzilla.NewClient(func() []byte {
			return []byte(cfg.Credentials.DecodedAPIKey())
		}, bugzillaEndpoint).WithCGIClient(cfg.Credentials.DecodedUsername(), cfg.Credentials.DecodedPassword()))
		if debug {
			return &loggingReadOnlyClient{delegate: c, slackLoggingClient: slackDebugClient}
		}
		return c
	}
}
