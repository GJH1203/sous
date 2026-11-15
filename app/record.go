package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GJH1203/sous/app/extract"
	"github.com/GJH1203/sous/app/store"
)

// draft is a record that waits for its author to say "对".
type draft struct {
	record extract.Record
	text   string    // the message it came from
	fixes  []string  // the corrections to it, in order
	at     time.Time // when the message came, in the app's zone
}

// draftTTL is how long a draft waits. After that, a message is more likely
// about something else than an answer to it, and must not be taken as a
// correction.
const draftTTL = 30 * time.Minute

var (
	yes = []string{"对", "对的", "是", "是的", "好", "好的", "确认", "保存", "ok"}
	no  = []string{"算了", "取消", "不记了", "不要了", "放弃"}
)

// record reads a record out of text and asks the person to confirm it. A
// draft that was still waiting is dropped: a new 记 means they moved on.
func (a *App) record(ctx context.Context, u *user, text string, now time.Time) string {
	if text == "" {
		return "要记什么？比如「记 今天做了番茄炒蛋，8 分」。"
	}
	var dropped string
	if u.draft != nil {
		dropped = fmt.Sprintf("（上一条「%s」没保存，已放弃。）\n", u.draft.record.Dish)
		u.draft = nil
	}
	r, err := extract.Extract(ctx, a.models, text, now)
	if err != nil {
		return dropped + a.trouble(u, err,
			"没看懂这条记录，没有记下。换个说法再发一次，比如「记 今天做了番茄炒蛋，8 分」。",
			"现在叫不到模型，这条没有记下。过一会儿再发一次。")
	}
	u.draft = &draft{record: r, text: text, at: now}
	return dropped + confirmation(r, now)
}

// answer replies to a message that is not a command: to the draft, if
// there is one.
func (a *App) answer(ctx context.Context, u *user, text string, now time.Time, expired bool) string {
	switch {
	case u.draft != nil && is(text, yes):
		return a.save(ctx, u, now)
	case u.draft != nil && is(text, no):
		dish := u.draft.record.Dish
		u.draft = nil
		return fmt.Sprintf("好，「%s」不记了。", dish)
	case u.draft != nil:
		return a.revise(ctx, u, text)
	case expired:
		return "上一条记录等了太久，已经放弃了。要记的话，重新发「记 …」。"
	case is(text, yes) || is(text, no):
		return "现在没有等着确认的记录。"
	default:
		return help
	}
}

// save stores the draft.
func (a *App) save(ctx context.Context, u *user, now time.Time) string {
	d := u.draft
	r := d.record
	times, err := a.store.SaveCook(ctx, store.Cook{
		Dish:        r.Dish,
		Ingredients: r.Ingredients,
		Rating:      r.Rating,
		Note:        r.Note,
		CookedOn:    r.Date,
		AuthorID:    u.id,
		// The corrections too: they are part of what the person said, and
		// without them the record could not be read out of the source again.
		Source: strings.Join(append([]string{d.text}, d.fixes...), "\n"),
	}, now)
	if err != nil {
		a.log.Error("saving a cook", "author", u.id, "err", err)
		return "保存失败了，记录还在。过一会儿再回「对」试试。"
	}
	u.draft = nil
	a.log.Info("cook saved", "author", u.id, "times", times)
	if times == 1 {
		return fmt.Sprintf("记好了。第一次做%s。", r.Dish)
	}
	return fmt.Sprintf("记好了。这是第 %d 次做%s。", times, r.Dish)
}

// revise applies a correction to the draft. If the model cannot, the draft
// stays as it was.
func (a *App) revise(ctx context.Context, u *user, fix string) string {
	d := u.draft
	// d.at, not now: "yesterday" in the correction means the day before the
	// message, like it did in the message.
	r, err := extract.Revise(ctx, a.models, d.text, d.record, fix, d.at)
	if err != nil {
		return a.trouble(u, err,
			"没看懂这条修改，记录还是上面那样。回「对」保存，回「算了」放弃，或者换个说法再改。",
			"现在叫不到模型，没改成，记录还是上面那样。过一会儿再说一次。")
	}
	d.record = r
	d.fixes = append(d.fixes, fix)
	return "改好了。\n" + confirmation(r, d.at)
}

// trouble logs an error from extract and picks the reply for it.
func (a *App) trouble(u *user, err error, notUnderstood, unreachable string) string {
	if errors.Is(err, extract.ErrNotUnderstood) {
		// Not the error itself: the problems it lists quote the model's
		// reply, which quotes the message.
		a.log.Warn("no valid record from the model", "author", u.id)
		return notUnderstood
	}
	a.log.Error("calling the model", "author", u.id, "err", err)
	return unreachable
}

// is reports whether text is one of words, give or take letter case and
// closing punctuation: "对。" and "OK!" count.
func is(text string, words []string) bool {
	return slices.Contains(words, strings.ToLower(strings.TrimRight(text, "。.！!~～ ")))
}

// confirmation shows r the way the person checks it, and asks.
func confirmation(r extract.Record, today time.Time) string {
	rating := "没打分"
	if r.Rating != nil {
		rating = strconv.FormatFloat(*r.Rating, 'f', -1, 64) + " 分"
	}
	note := r.Note
	if note == "" {
		note = "无"
	}
	return "记下这条吗？\n" +
		"菜：" + r.Dish + "\n" +
		"食材：" + strings.Join(r.Ingredients, "、") + "\n" +
		"评分：" + rating + "\n" +
		"备注：" + note + "\n" +
		"日期：" + day(r.Date, today) + "\n" +
		"回「对」保存，回「算了」放弃，或者直接说哪里要改。"
}

var weekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// day writes a YYYY-MM-DD date the way people say it, such as
// "11 月 13 日 周五（昨天）": a wrong date is easier to spot that way.
func day(date string, today time.Time) string {
	d, err := time.ParseInLocation(time.DateOnly, date, today.Location())
	if err != nil {
		return date // extract has checked it, so this does not happen
	}
	s := fmt.Sprintf("%d 月 %d 日 %s", int(d.Month()), d.Day(), weekdays[d.Weekday()])
	if d.Year() != today.Year() {
		s = fmt.Sprintf("%d 年 ", d.Year()) + s
	}
	for i, word := range []string{"今天", "昨天", "前天"} {
		if today.AddDate(0, 0, -i).Format(time.DateOnly) == date {
			return s + "（" + word + "）"
		}
	}
	return s
}
