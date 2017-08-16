package alb

import (
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	awsutil "github.com/coreos/alb-ingress-controller/pkg/util/aws"
	"github.com/coreos/alb-ingress-controller/pkg/util/log"
	util "github.com/coreos/alb-ingress-controller/pkg/util/types"
	api "k8s.io/api/core/v1"
)

// Rule contains a current/desired Rule
type Rule struct {
	CurrentRule *elbv2.Rule
	DesiredRule *elbv2.Rule
	svcName     string
	deleted     bool
	logger      *log.Logger
}

// NewRule returns an alb.Rule based on the provided parameters.
func NewRule(priority int, hostname, path, svcname string, logger *log.Logger) *Rule {
	r := &elbv2.Rule{
		Actions: []*elbv2.Action{
			{
				TargetGroupArn: nil, // Populated at creation, since we create rules before we create rules
				Type:           aws.String("forward"),
			},
		},
	}

	if priority == 0 {
		r.IsDefault = aws.Bool(true)
		r.Priority = aws.String("default")
	} else {
		r.IsDefault = aws.Bool(false)
		r.Priority = aws.String(fmt.Sprintf("%v", priority))
	}

	if hostname != "" {
		r.Conditions = append(r.Conditions, &elbv2.RuleCondition{
			Field:  aws.String("host-header"),
			Values: []*string{aws.String(hostname)},
		})
	}

	if path != "" {
		r.Conditions = append(r.Conditions, &elbv2.RuleCondition{
			Field:  aws.String("path-pattern"),
			Values: []*string{aws.String(path)},
		})
	}

	rule := &Rule{
		svcName:     svcname,
		DesiredRule: r,
		logger:      logger,
	}
	return rule
}

// NewRuleFromAWSRule creates a Rule from an elbv2.Rule
func NewRuleFromAWSRule(r *elbv2.Rule, logger *log.Logger) *Rule {
	rule := &Rule{
		CurrentRule: r,
		logger:      logger,
	}
	return rule
}

// Reconcile compares the current and desired state of this Rule instance. Comparison
// results in no action, the creation, the deletion, or the modification of an AWS Rule to
// satisfy the ingress's current state.
func (r *Rule) Reconcile(rOpts *ReconcileOptions, l *Listener) error {
	switch {
	case r.DesiredRule == nil: // rule should be deleted
		if r.CurrentRule == nil {
			break
		}
		if *r.CurrentRule.IsDefault == true {
			break
		}
		r.logger.Infof("Start Rule deletion.")
		if err := r.delete(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "DELETE", "%s rule deleted", *r.CurrentRule.Priority)
		r.logger.Infof("Completed Rule deletion. Rule: %s | Condition: %s",
			log.Prettify(r.CurrentRule.RuleArn),
			log.Prettify(r.CurrentRule.Conditions))

	case *r.DesiredRule.IsDefault: // rule is default (attached to listener), do nothing
		r.logger.Debugf("Found desired rule that is a default and is already created with its respective listener. Rule: %s",
			log.Prettify(r.DesiredRule))
		r.CurrentRule = r.DesiredRule

	case r.CurrentRule == nil: // rule doesn't exist and should be created
		r.logger.Infof("Start Rule creation.")
		if err := r.create(rOpts, l); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "CREATE", "%s rule created", *r.CurrentRule.Priority)
		r.logger.Infof("Completed Rule creation. Rule Priority: %s | Condition: %s",
			log.Prettify(r.CurrentRule.Priority),
			log.Prettify(r.CurrentRule.Conditions))

	case r.needsModification(): // diff between current and desired, modify rule
		r.logger.Infof("Start Rule modification.")
		if err := r.modify(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "MODIFY", "%s rule modified", *r.CurrentRule.Priority)
		r.logger.Infof("Completed Rule modification. [UNIMPLEMENTED]")

	default:
		r.logger.Debugf("No listener modification required.")
	}

	return nil
}

func (r *Rule) targetGroupArn(tgs TargetGroups) *string {
	// Despite it being a list, i think you can only have one action per rule
	if r.CurrentRule != nil && r.CurrentRule.Actions[0].TargetGroupArn != nil {
		return r.CurrentRule.Actions[0].TargetGroupArn
	}
	tgIndex := tgs.LookupBySvc(r.svcName)
	if tgIndex < 0 {
		r.logger.Errorf("Failed to locate TargetGroup related to this service: %s", r.svcName)
		return nil
	}
	return tgs[tgIndex].CurrentTargetGroup.TargetGroupArn
}

func (r *Rule) create(rOpts *ReconcileOptions, l *Listener) error {
	in := &elbv2.CreateRuleInput{
		Actions:     r.DesiredRule.Actions,
		Conditions:  r.DesiredRule.Conditions,
		ListenerArn: l.CurrentListener.ListenerArn,
		Priority:    priority(r.DesiredRule.Priority),
	}

	in.Actions[0].TargetGroupArn = r.targetGroupArn(rOpts.loadbalancer.TargetGroups)

	o, err := awsutil.ALBsvc.CreateRule(in)
	if err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error creating %v rule: %s", *in.Priority, err.Error())
		r.logger.Errorf("Failed Rule creation. Rule: %s | Error: %s",
			log.Prettify(r.DesiredRule), err.Error())
		return err
	}
	r.CurrentRule = o.Rules[0]

	return nil
}

func (r *Rule) modify(rOpts *ReconcileOptions) error {
	// TODO: Unimplemented
	return nil
}

func (r *Rule) delete(rOpts *ReconcileOptions) error {
	if r.CurrentRule == nil {
		r.logger.Infof("Rule entered delete with no CurrentRule to delete. Rule: %s",
			log.Prettify(r))
		return nil
	}

	// If the current rule was a default, it's bound to the listener and won't be deleted from here.
	if *r.CurrentRule.IsDefault {
		r.logger.Debugf("Deletion hit for default rule, which is bound to the Listener. It will not be deleted from here. Rule. Rule: %s",
			log.Prettify(r))
		return nil
	}

	in := &elbv2.DeleteRuleInput{RuleArn: r.CurrentRule.RuleArn}
	if _, err := awsutil.ALBsvc.DeleteRule(in); err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error deleting %s rule: %s", *r.CurrentRule.Priority, err.Error())
		r.logger.Infof("Failed Rule deletion. Error: %s", err.Error())
		return err
	}

	r.deleted = true
	return nil
}

func (r *Rule) needsModification() bool {
	cr := r.CurrentRule
	dr := r.DesiredRule

	switch {
	case cr == nil:
		return true
		// TODO: If we can populate the TargetGroupArn in NewALBIngressFromIngress, we can enable this
		// case awsutil.Prettify(cr.Actions) != awsutil.Prettify(dr.Actions):
		// 	return true
	case log.Prettify(cr.Conditions) != log.Prettify(dr.Conditions):
		return true
	}

	return false
}

// CurrentEquals returns true if the two CurrentRule and target rule are the same
func (r *Rule) CurrentEquals(target *elbv2.Rule) bool {
	switch {
	case r.CurrentRule == nil:
		return false
	case !util.DeepEqual(r.CurrentRule.Priority, target.Priority):
		return false
	case !util.DeepEqual(r.CurrentRule.Conditions, target.Conditions):
		return false
	}
	return true
}

func priority(s *string) *int64 {
	if *s == "default" {
		return aws.Int64(0)
	}
	i, _ := strconv.ParseInt(*s, 10, 64)
	return aws.Int64(i)
}
