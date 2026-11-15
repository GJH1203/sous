package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GJH1203/sous/app/store"
)

// maxListed keeps a reply well inside Discord's limit on message length.
const maxListed = 10

// want answers "想吃 豆腐": the dishes cooked with it, best first. It is
// all SQL; models are for writing, not for reading.
func (a *App) want(ctx context.Context, ingredient string, now time.Time) string {
	if ingredient == "" {
		return "想吃什么？比如「想吃 豆腐」。"
	}
	dishes, err := a.store.DishesWith(ctx, ingredient)
	if err != nil {
		a.log.Error("finding dishes", "err", err)
		return "查询失败了，过一会儿再试。"
	}
	if len(dishes) == 0 {
		return fmt.Sprintf("还没做过带「%s」的菜。", ingredient)
	}
	dishes = rank(dishes)
	var b strings.Builder
	fmt.Fprintf(&b, "做过的带「%s」的菜：", ingredient)
	for i, d := range dishes[:min(len(dishes), maxListed)] {
		fmt.Fprintf(&b, "\n%d. %s：做过 %d 次，%s，上次是%s", i+1, d.Dish, d.Times, score(d.Rating), since(d.Last, now))
		if d.Note != "" {
			fmt.Fprintf(&b, "。最近的备注：%s", d.Note)
		}
	}
	if len(dishes) > maxListed {
		fmt.Fprintf(&b, "\n还有 %d 道没列出来。", len(dishes)-maxListed)
	}
	return b.String()
}

// rank orders dishes for "想吃": the best rated first, and among equal
// ratings the one not cooked for the longest, for some variety. Dishes
// never rated come last. It returns a sorted copy.
func rank(dishes []store.DishStats) []store.DishStats {
	out := slices.Clone(dishes)
	slices.SortStableFunc(out, func(x, y store.DishStats) int {
		if c := byRating(x.Rating, y.Rating); c != 0 {
			return c
		}
		if c := strings.Compare(x.Last, y.Last); c != 0 {
			return c // YYYY-MM-DD sorts as text: the earlier day first
		}
		return strings.Compare(x.Dish, y.Dish)
	})
	return out
}

// byRating puts the higher rating first, and no rating last. Ratings are
// compared as shown, to one decimal: 8.47 and 8.5 both show as 8.5, and a
// person would call them equal.
func byRating(x, y *float64) int {
	switch {
	case x == nil && y == nil:
		return 0
	case x == nil:
		return 1
	case y == nil:
		return -1
	}
	return cmp.Compare(round(*y), round(*x))
}

func round(r float64) float64 { return math.Round(r*10) / 10 }

func score(r *float64) string {
	if r == nil {
		return "没打过分"
	}
	return "平均 " + strconv.FormatFloat(round(*r), 'f', -1, 64) + " 分"
}

// since says how long ago date was, as seen from now.
func since(date string, now time.Time) string {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return date // the schema checks it, so this does not happen
	}
	// Both as midnight UTC, where every day is 24 hours long.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch days := int(today.Sub(d).Hours() / 24); days {
	case 0:
		return "今天"
	case 1:
		return "昨天"
	default:
		return fmt.Sprintf("%d 天前（%d 月 %d 日）", days, int(d.Month()), d.Day())
	}
}

const aliasUsage = "格式是「别名 西红柿 = 番茄」：左边是另一个叫法，右边是要算作的食材。只发「别名」会列出所有别名。"

// alias handles "别名 西红柿 = 番茄", and "别名" alone, which lists them.
func (a *App) alias(ctx context.Context, text string) string {
	if text == "" {
		return a.aliases(ctx)
	}
	alias, name, ok := strings.Cut(strings.ReplaceAll(text, "＝", "="), "=")
	alias, name = strings.TrimSpace(alias), strings.TrimSpace(name)
	if !ok || alias == "" || name == "" || strings.Contains(name, "=") {
		return aliasUsage
	}
	canonical, moved, err := a.store.AddAlias(ctx, alias, name)
	switch {
	case errors.Is(err, store.ErrSameIngredient):
		return fmt.Sprintf("「%s」和「%s」已经算同一种了。", alias, name)
	case err != nil:
		a.log.Error("adding an alias", "err", err)
		return "保存失败了，过一会儿再试。"
	case moved > 0:
		return fmt.Sprintf("好，以后「%s」就算「%s」。之前记成「%s」的 %d 次也改过来了。", alias, canonical, alias, moved)
	}
	return fmt.Sprintf("好，以后「%s」就算「%s」。", alias, canonical)
}

func (a *App) aliases(ctx context.Context) string {
	list, err := a.store.Aliases(ctx)
	if err != nil {
		a.log.Error("listing aliases", "err", err)
		return "查询失败了，过一会儿再试。"
	}
	if len(list) == 0 {
		return "还没有别名。" + aliasUsage
	}
	var b strings.Builder
	b.WriteString("别名：")
	for _, al := range list {
		fmt.Fprintf(&b, "\n%s = %s", al.Alias, al.Name)
	}
	return b.String()
}
