// Package i18n holds every user-visible text in English and Russian. The
// language is chosen once in the config (lang = ru | en).
package i18n

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// T returns the message for key in lang, formatted with args.
func T(lang, key string, args ...any) string {
	m, ok := messages[key]
	if !ok {
		return key
	}
	s := m[0]
	if lang == "ru" {
		s = m[1]
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// Has reports whether key exists.
func Has(key string) bool {
	_, ok := messages[key]
	return ok
}

// Keys returns all keys (for tests).
func Keys() map[string][2]string { return messages }

// Duration formats d as "3 h 05 min" / "3 ч 05 мин", rounded down to minutes.
func Duration(lang string, d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d / time.Minute)
	if m < 60 {
		return T(lang, "dur.min", m)
	}
	return T(lang, "dur.hmin", m/60, m%60)
}

// Span formats a long stretch of time: like Duration below 24 hours, then
// whole days and hours ("2 d 05 h" / "2 д 05 ч"), rounded down.
func Span(lang string, d time.Duration) string {
	if d < 24*time.Hour {
		return Duration(lang, d)
	}
	h := int(d / time.Hour)
	return T(lang, "dur.dh", h/24, h%24)
}

// Plural returns key+".one", ".few" or ".many" for n, formatted with n.
// English uses one/many; Russian one (1, 21…), few (2–4, 22–24…), many.
func Plural(lang, key string, n int) string {
	form := "many"
	if lang == "ru" {
		switch m10, m100 := n%10, n%100; {
		case m10 == 1 && m100 != 11:
			form = "one"
		case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
			form = "few"
		}
	} else if n == 1 {
		form = "one"
	}
	return T(lang, key+"."+form, n)
}

// Num formats x with dec decimals: "1 811", "4,6" in Russian (non-breaking
// space between thousands, decimal comma) and "1,811", "4.6" in English.
func Num(lang string, x float64, dec int) string {
	neg := x < 0
	if neg {
		x = -x
	}
	s := strconv.FormatFloat(x, 'f', dec, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	sep, point := ",", "."
	if lang == "ru" {
		sep, point = "\u00a0", ","
	}
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(r)
	}
	out := b.String()
	if frac != "" {
		out += point + frac
	}
	if neg && strings.Trim(s, "0.") != "" {
		out = "-" + out
	}
	return out
}

// Day formats t as "31 авг" / "Aug 31"; with year, "31 авг 2026" / "Aug 31, 2026".
func Day(lang string, t time.Time, year bool) string {
	mon := T(lang, fmt.Sprintf("mon.%d", int(t.Month())))
	switch {
	case lang == "ru" && year:
		return fmt.Sprintf("%d %s %d", t.Day(), mon, t.Year())
	case lang == "ru":
		return fmt.Sprintf("%d %s", t.Day(), mon)
	case year:
		return fmt.Sprintf("%s %d, %d", mon, t.Day(), t.Year())
	default:
		return fmt.Sprintf("%s %d", mon, t.Day())
	}
}

// Reason returns the label for a visit or session end reason.
func Reason(lang, reason string) string {
	if reason == "" {
		reason = "open"
	}
	return T(lang, "reason."+reason)
}

// messages maps key -> {English, Russian}.
var messages = map[string][2]string{
	// Durations
	"dur.min":  {"%d min", "%d мин"},
	"dur.hmin": {"%d h %02d min", "%d ч %02d мин"},
	"dur.dh":   {"%d d %02d h", "%d д %02d ч"},

	// How a visit ended
	"reason.idle_logout": {"idle logout", "выход по бездействию"},
	"reason.logout":      {"logout", "выход из системы"},
	"reason.timeout":     {"PC stopped responding", "компьютер перестал отвечать"},
	"reason.replaced":    {"PC restarted", "компьютер перезапущен"},
	"reason.auto_closed": {"nightly auto-close", "ночное автозакрытие"},
	"reason.checkout":    {"checked out in Telegram", "уход отмечен в Telegram"},
	"reason.moved":       {"checked in on another PC", "отметка на другом компьютере"},
	"reason.taken_over":  {"another student checked in on this PC", "на этом компьютере отметился другой ученик"},
	"reason.manual":      {"manual", "вручную"},
	"reason.open":        {"in progress", "идёт"},

	// Telegram bot
	"bot.linked":        {"Linked as %s ✅\nFrom now on, scan the QR code on a school PC screen to check in.", "Аккаунт привязан: %s ✅\nТеперь, чтобы отметиться, сканируйте QR-код на экране школьного компьютера."},
	"bot.already_same":  {"You're already linked as %s.", "Вы уже привязаны как %s."},
	"bot.tg_taken":      {"This Telegram account is already linked to %s. If that's wrong, ask staff.", "Этот аккаунт Telegram уже привязан к ученику %s. Если это ошибка, обратитесь к сотрудникам."},
	"bot.student_taken": {"This student already has a linked Telegram account. Ask staff.", "К этому ученику уже привязан другой аккаунт Telegram. Обратитесь к сотрудникам."},
	"bot.code_invalid":  {"This link is not valid. Ask staff for a new one.", "Эта ссылка недействительна. Попросите у сотрудников новую."},
	"bot.code_used":     {"This link has already been used. Ask staff for a new one.", "Эта ссылка уже использована. Попросите у сотрудников новую."},
	"bot.code_expired":  {"This link has expired. Ask staff for a new one.", "Срок действия ссылки истёк. Попросите у сотрудников новую."},
	"bot.checked_in":    {"Checked in at %s ✅", "Вы отметились на %s ✅"},
	"bot.moved":         {"Your visit at %s was closed at %s.", "Посещение на %s закрыто в %s."},
	"bot.already_in":    {"You're already checked in at %s.", "Вы уже отмечены на %s."},
	"bot.expired":       {"Code expired, scan again.", "Код устарел, отсканируйте ещё раз."},
	"bot.used":          {"This code was already used. Scan the new code on the screen.", "Этот код уже использован. Отсканируйте новый код с экрана."},
	"bot.bad_token":     {"This code is not valid. Scan the code on the PC screen.", "Код недействителен. Отсканируйте код с экрана компьютера."},
	"bot.no_session":    {"Log in on the PC first.", "Сначала войдите в систему на компьютере."},
	"bot.not_linked":    {"Your account isn't linked, ask staff.", "Ваш аккаунт не привязан, обратитесь к сотрудникам."},
	"bot.checkout_done": {"Checked out at %s. Today: %s.", "Уход отмечен в %s. Сегодня: %s."},
	"bot.checkout_none": {"You have no open visit.", "У вас нет открытого посещения."},
	"bot.status_open":   {"Checked in at %s since %s.", "Вы отмечены на %s с %s."},
	"bot.status_none":   {"You're not checked in right now.", "Сейчас вы не отмечены."},
	"bot.status_totals": {"Today: %s.\nThis week: %s.", "Сегодня: %s.\nЗа эту неделю: %s."},
	"bot.closed":        {"Checked out at %s (%s, %s). Today: %s.", "Отметка закрыта в %s (%s, %s). Сегодня: %s."},
	"bot.rescan_hint":   {"If you're still at the PC, scan the QR code again.", "Если вы всё ещё за компьютером, отсканируйте QR-код ещё раз."},
	"bot.help":          {"Scan the QR code on a school PC screen to check in.\n\n/status — current visit, hours today and this week\n/checkout — check out now\n/id — your Telegram ID", "Чтобы отметиться, отсканируйте QR-код на экране школьного компьютера.\n\n/status — текущее посещение, часы за сегодня и за неделю\n/checkout — отметить уход сейчас\n/id — ваш Telegram ID"},
	"bot.welcome":       {"Hi! To link your account, open the personal link you got from staff.", "Здравствуйте! Чтобы привязать аккаунт, откройте личную ссылку, которую выдали сотрудники."},
	"bot.rate_limited":  {"Too many messages. Please wait a minute.", "Слишком много сообщений. Подождите минуту."},
	"bot.your_id":       {"Your Telegram ID: %d", "Ваш Telegram ID: %d"},
	"bot.error":         {"Something went wrong. Try again in a minute.", "Что-то пошло не так. Попробуйте через минуту."},

	// QR page on the PCs
	"qr.title":            {"Check-in", "Отметка присутствия"},
	"qr.scan":             {"Scan with your phone camera", "Отсканируйте камерой телефона"},
	"qr.steps":            {"Telegram opens → tap Start → wait for “Checked in”", "Откроется Telegram → нажмите «Старт» → дождитесь «Вы отметились»"},
	"qr.refresh_in":       {"New code in {s} s", "Новый код через {s} с"},
	"qr.login_first":      {"Log in first", "Сначала войдите в систему"},
	"qr.login_first_hint": {"This PC has no registered session yet. If you are logged in, wait a few seconds; if this stays, log out and log in again.", "У этого компьютера ещё нет зарегистрированного сеанса. Если вы уже вошли, подождите несколько секунд; если сообщение не исчезает, выйдите и войдите снова."},
	"qr.not_school_pc":    {"This page works only on school PCs.", "Эта страница работает только на школьных компьютерах."},
	"qr.offline":          {"No connection to the server. Retrying…", "Нет связи с сервером. Повторяем…"},
	"qr.checked_in":       {"Checked in on this PC", "Отмечены на этом компьютере"},

	// Admin: common
	"app.title":     {"Attendance", "Посещаемость"},
	"nav.today":     {"Today", "Сегодня"},
	"nav.hours":     {"Hours", "Часы"},
	"nav.students":  {"Students", "Ученики"},
	"nav.anomalies": {"Anomalies", "Аномалии"},
	"nav.platform":  {"Platform", "Платформа"},
	"nav.help":      {"Peer help", "Взаимопомощь"},
	"nav.audit":     {"Audit log", "Журнал"},
	"nav.logout":    {"Log out", "Выйти"},
	"role.admin":    {"admin", "администратор"},
	"role.teacher":  {"teacher (read-only)", "преподаватель (только просмотр)"},
	"c.save":        {"Save", "Сохранить"},
	"c.cancel":      {"Cancel", "Отмена"},
	"c.delete":      {"Delete", "Удалить"},
	"c.edit":        {"Edit", "Изменить"},
	"c.add":         {"Add", "Добавить"},
	"c.show":        {"Show", "Показать"},
	"c.reason":      {"Reason", "Причина"},
	"c.reason_ph":   {"Why? (required)", "Почему? (обязательно)"},
	"c.start":       {"Start", "Начало"},
	"c.end":         {"End", "Конец"},
	"c.pc":          {"PC", "Компьютер"},
	"c.student":     {"Student", "Ученик"},
	"c.group":       {"Group", "Группа"},
	"c.all_groups":  {"All groups", "Все группы"},
	"c.name":        {"Name", "Имя"},
	"c.date":        {"Date", "Дата"},
	"c.duration":    {"Duration", "Длительность"},
	"c.ended":       {"How it ended", "Как закончилось"},
	"c.actions":     {"Actions", "Действия"},
	"c.none":        {"Nothing here.", "Здесь пока пусто."},
	"c.when":        {"When", "Когда"},
	"c.who":         {"Who", "Кто"},
	"c.what":        {"What", "Что"},
	"c.details":     {"Details", "Подробности"},
	"c.from":        {"From", "С"},
	"c.to":          {"To", "По"},
	"c.prev":        {"← Previous", "← Назад"},
	"c.next":        {"Next →", "Вперёд →"},
	"c.now":         {"Current", "Текущий"},
	"c.ago":         {"%s ago", "%s назад"},
	"c.never":       {"never", "никогда"},
	"c.just_now":    {"just now", "только что"},
	"c.print":       {"Print", "Печать"},
	"corr.add":      {"added", "добавлено"},
	"corr.edit":     {"edited", "изменено"},
	"corr.delete":   {"deleted", "удалено"},
	"c.yes":         {"yes", "да"},
	"c.no":          {"no", "нет"},
	"c.done":        {"Done.", "Готово."},
	"c.forbidden":   {"Your role cannot do this.", "Ваша роль не позволяет это сделать."},
	"c.error":       {"Error: %s", "Ошибка: %s"},

	// Weekdays (time.Weekday order, Sunday first)
	"mon.1":  {"Jan", "янв"},
	"mon.2":  {"Feb", "фев"},
	"mon.3":  {"Mar", "мар"},
	"mon.4":  {"Apr", "апр"},
	"mon.5":  {"May", "мая"},
	"mon.6":  {"Jun", "июн"},
	"mon.7":  {"Jul", "июл"},
	"mon.8":  {"Aug", "авг"},
	"mon.9":  {"Sep", "сен"},
	"mon.10": {"Oct", "окт"},
	"mon.11": {"Nov", "ноя"},
	"mon.12": {"Dec", "дек"},

	// UI switcher (both admin UIs) and the new analytics dashboard
	"ui.dashboard": {"Dashboard", "Дашборд"},
	"ui.admin":     {"Admin", "Админка"},
	"ui.switch":    {"Interface", "Интерфейс"},
	"ui.sections":  {"Sections", "Разделы"},
	"ui.old":       {"Old interface", "Старый интерфейс"},
	"ui.theme":     {"Switch light/dark theme", "Сменить светлую/тёмную тему"},

	"dash.analytics":        {"Analytics", "Аналитика"},
	"dash.title":            {"Student attendance", "Посещаемость студентов"},
	"dash.updated":          {"updated %s", "обновлено %s"},
	"dash.from":             {"from", "с"},
	"dash.to":               {"to", "по"},
	"dash.all":              {"All", "Все"},
	"dash.active":           {"Active", "Активные"},
	"dash.left":             {"Left", "Выбывшие"},
	"dash.reset":            {"Reset", "Сбросить"},
	"dash.k_students":       {"Students", "Студентов"},
	"dash.k_students_sub":   {"active now: %d", "активны сейчас: %d"},
	"dash.k_hours":          {"Hours in school", "Часов в школе"},
	"dash.k_sessions":       {"Sessions", "Сессий"},
	"dash.k_sessions_sub":   {"avg %s h per session", "ср. %s ч на сессию"},
	"dash.k_per_day":        {"Hours per student per day", "Часов на студента в день"},
	"dash.k_per_day_sub":    {"on days present", "в днях присутствия"},
	"dash.k_att":            {"Average attendance", "Средняя посещаемость"},
	"dash.k_att_sub":        {"days present out of school days", "дней присутствия из учебных"},
	"dash.k_arrival":        {"Average arrival", "Средний приход"},
	"dash.k_arrival_sub":    {"start of the first session of the day", "время первой сессии дня"},
	"dash.h":                {"h", "ч"},
	"dash.school_days.one":  {"%d school day", "%d учебный день"},
	"dash.school_days.few":  {"%d school days", "%d учебных дня"},
	"dash.school_days.many": {"%d school days", "%d учебных дней"},
	"dash.n_students.one":   {"%d student", "%d студент"},
	"dash.n_students.few":   {"%d students", "%d студента"},
	"dash.n_students.many":  {"%d students", "%d студентов"},
	"dash.i_retention":      {"Retention:", "Удержание:"},
	"dash.i_retention_v":    {"%d of %d students (%d%%)", "%d из %d студентов (%d%%)"},
	"dash.i_retention_t":    {"were in school in the last %d days of the period.", "были в школе за последние %d дня периода."},
	"dash.i_top":            {"Most hours:", "Больше всех часов:"},
	"dash.i_top_t":          {"— %s h in %d d.", "— %s ч за %d дн."},
	"dash.i_peak":           {"Peak day —", "Пиковый день —"},
	"dash.i_peak_t":         {": %s h, %s. Peak load —", ": %s ч, %s. Пик загрузки —"},
	"dash.c_hours":          {"Hours in school by day", "Часы в школе по дням"},
	"dash.c_hours_note":     {"total hours of all students · lighter: weekends", "сумма часов всех студентов · светлые — выходные"},
	"dash.c_students":       {"Students per day", "Студентов в день"},
	"dash.c_students_note":  {"unique visits", "уникальные посещения"},
	"dash.c_hourly":         {"Load by hour of day", "Загрузка по часам суток"},
	"dash.c_hourly_note":    {"avg students in school", "ср. число студентов в школе"},
	"dash.c_weekday":        {"By day of week", "По дням недели"},
	"dash.c_weekday_note":   {"avg hours per present student", "ср. часов на присутствующего студента"},
	"dash.matrix":           {"Attendance matrix", "Матрица посещаемости"},
	"dash.l_none":           {"none", "нет"},
	"dash.l_lt2":            {"< 2 h", "< 2 ч"},
	"dash.students":         {"Students", "Студенты"},
	"dash.search":           {"Search by name", "Поиск по имени"},
	"dash.export":           {"Export CSV", "Экспорт CSV"},
	"dash.all_groups":       {"All groups", "Все группы"},
	"dash.per_page":         {"Rows", "Строк"},
	"dash.per_all":          {"All", "Все"},
	"dash.shown":            {"%d–%d of %d", "%d–%d из %d"},
	"dash.pages":            {"Pages", "Страницы"},
	"dash.prev":             {"Previous page", "Предыдущая страница"},
	"dash.next":             {"Next page", "Следующая страница"},
	"dash.show":             {"Show", "Показать"},
	"dash.no_match":         {"No students match these filters.", "Нет студентов с такими фильтрами."},

	// Anomalies of the new UI
	"help.title":             {"Peer help", "Взаимопомощь"},
	"help.intro":             {"How often students are named in the weekly form \"who helped you most\". Typed logins are matched to students' and platform logins; small typos are corrected, anything doubtful is listed for review.", "Как часто учеников называют в еженедельной форме «кто вам больше всего помог». Введённые логины сверяются с логинами учеников и платформы; мелкие опечатки исправляются, всё сомнительное попадает в список на проверку."},
	"help.cohort_label":      {"Important", "Важно"},
	"help.no_cohort":         {"Only your students' logins are known. Load all participants of the current piscine on the Platform tab: otherwise a classmate who is not in this system is not recognised, and a login one letter away from a student's may be counted for that student.", "Сейчас известны только логины ваших учеников. Загрузите всех участников текущего бассейна на вкладке «Платформа»: иначе одноклассник, которого нет в этой системе, не распознаётся, а логин, отличающийся от логина ученика одной буквой, может засчитаться этому ученику."},
	"help.import":            {"Import a week's answers", "Загрузить ответы за неделю"},
	"help.file":              {"Answers (.xlsx or .csv)", "Ответы (.xlsx или .csv)"},
	"help.week_of":           {"Week (any day of it)", "Неделя (любой её день)"},
	"help.import_btn":        {"Import", "Загрузить"},
	"help.import_hint":       {"Download the form's answer sheet as .xlsx or .csv. Importing a week again replaces it. Only the logins and names are kept, not the comments.", "Скачайте таблицу ответов формы в .xlsx или .csv. Повторная загрузка той же недели заменяет её. Сохраняются только логины и имена, комментарии — нет."},
	"help.none":              {"No week has been imported yet.", "Пока не загружено ни одной недели."},
	"help.counts":            {"Times named", "Сколько раз назвали"},
	"help.week":              {"Week", "Неделя"},
	"help.week_label":        {"Week of %s", "Неделя с %s"},
	"help.all_weeks":         {"All weeks", "Все недели"},
	"help.week_stats":        {"%d answers, %d counted (a later answer of the same person replaces the earlier one) · %d mentions counted · %d unresolved", "Ответов: %d, учтено: %d (более поздний ответ того же человека заменяет прежний) · упоминаний учтено: %d · не распознано: %d"},
	"help.col_week":          {"This week", "За неделю"},
	"help.col_total":         {"All weeks", "За все недели"},
	"help.not_student":       {"not a student here", "не ученик в системе"},
	"help.nobody_named":      {"Nobody is named in this selection.", "В этой выборке никого не назвали."},
	"help.review":            {"To check", "На проверку"},
	"help.review_hint":       {"Logins that were not typed exactly. Corrected ones already count; unresolved ones do not count until you give them a login. A saved login is used for every week.", "Логины, введённые не точно. Исправленные уже учтены; нераспознанные не учитываются, пока вы не укажете логин. Сохранённый логин применяется ко всем неделям."},
	"help.review_none":       {"Every login this week was typed exactly.", "На этой неделе все логины введены точно."},
	"help.typed":             {"Typed", "Введено"},
	"help.times":             {"Times", "Раз"},
	"help.result":            {"Counted as", "Учтено как"},
	"help.by":                {"Written by", "Кто написал"},
	"help.fix":               {"Login", "Логин"},
	"help.how_typo":          {"typo, corrected:", "опечатка, исправлено:"},
	"help.how_name":          {"a name:", "по имени:"},
	"help.how_alias":         {"set by staff:", "указано сотрудником:"},
	"help.how_exact":         {"exact:", "точно:"},
	"help.unresolved":        {"not counted", "не учтено"},
	"help.maybe":             {"maybe", "возможно"},
	"help.delete":            {"Delete the week of %s", "Удалить неделю с %s"},
	"help.delete_confirm":    {"Delete the answers of the week of %s?", "Удалить ответы за неделю с %s?"},
	"help.imported":          {"%d answers imported for the week of %s.", "Загружено ответов: %d, неделя с %s."},
	"help.replaced":          {"The earlier import of this week was replaced.", "Прежняя загрузка этой недели заменена."},
	"help.deleted":           {"The week is deleted.", "Неделя удалена."},
	"help.alias_saved":       {"Saved. It applies to every week.", "Сохранено. Применяется ко всем неделям."},
	"help.no_file":           {"Choose the file with the answers.", "Выберите файл с ответами."},
	"help.no_columns":        {"No peer columns found: the first row must have headers such as \"Peer #1\".", "Не найдены столбцы с логинами: в первой строке должны быть заголовки вроде «Peer #1»."},
	"help.too_big":           {"The file is larger than 10 MB.", "Файл больше 10 МБ."},
	"help.not_workbook":      {"This file cannot be read: save it as .xlsx or .csv.", "Этот файл не читается: сохраните его как .xlsx или .csv."},
	"help.bad_week":          {"Choose the week.", "Выберите неделю."},
	"help.unknown_login":     {"No student or platform user has this login.", "Ни у ученика, ни у пользователя платформы нет такого логина."},
	"help.on_student":        {"Named in the peer-help form", "Назвали в форме взаимопомощи"},
	"help.student_total":     {"%d times in all weeks", "%d раз за все недели"},
	"help.student_none":      {"Not named yet.", "Пока не называли."},
	"plat.title":             {"Learning platform", "Учебная платформа"},
	"plat.intro":             {"The students' levels, XP and audit ratio, read from the learning platform by their logins.", "Уровни, XP и аудит-рейтинг учеников, прочитанные с учебной платформы по их логинам."},
	"plat.status":            {"Sync", "Синхронизация"},
	"plat.sync_now":          {"Sync now", "Синхронизировать сейчас"},
	"plat.link":              {"Link students by name", "Связать учеников по имени"},
	"plat.link_confirm":      {"Give a login to each student without one whose name matches exactly one platform user? Every change goes to the journal and can be edited on the student page.", "Дать логин каждому ученику без логина, чьё имя совпадает ровно с одним пользователем платформы? Каждое изменение попадёт в журнал, его можно исправить на странице ученика."},
	"plat.no_platform":       {"The platform address is empty in attendance.conf (platform_url), so nothing is read.", "В attendance.conf пустой адрес платформы (platform_url), поэтому ничего не читается."},
	"plat.need_token":        {"Save the access token under Settings to start.", "Чтобы начать, сохраните токен доступа в разделе «Настройки»."},
	"plat.logins_only":       {"the students' logins are read", "читаются логины учеников"},
	"plat.nothing_to_read":   {"Nothing to read: no student has a platform login yet, and no event is loaded.", "Нечего читать: ни у одного ученика пока нет логина на платформе, и ни одно событие не загружается."},
	"plat.events_title":      {"Events", "События"},
	"plat.events_intro":      {"Events your students are in, found by their logins. Load all participants of an event, such as the current piscine, so that \"Link students by name\" and peer help know everyone in it.", "События, в которых участвуют ваши ученики (найдены по их логинам). Загрузите всех участников события, например текущего бассейна, чтобы «Связать учеников по имени» и взаимопомощь знали всех его участников."},
	"plat.col_event":         {"Event", "Событие"},
	"plat.col_number":        {"Number", "Номер"},
	"plat.col_dates":         {"Dates", "Даты"},
	"plat.col_ours":          {"Our students", "Наших учеников"},
	"plat.col_read":          {"Read from the platform", "Прочитано с платформы"},
	"plat.col_level":         {"Level", "Уровень"},
	"plat.load_all":          {"Load all participants", "Загружать всех участников"},
	"plat.load_stop":         {"Stop loading all", "Не загружать всех"},
	"plat.loading":           {"all participants", "все участники"},
	"plat.event_unknown":     {"not synced yet, or not on the platform", "ещё не синхронизировано или нет на платформе"},
	"plat.event_saved":       {"Saved. Press \"Sync now\" to read it now.", "Сохранено. Нажмите «Синхронизировать сейчас», чтобы прочитать сразу."},
	"plat.current":           {"current", "текущее"},
	"plat.no_event":          {"in no event", "ни в одном событии"},
	"plat.login_wait":        {"The login %s is read at the first sync.", "Логин %s будет прочитан при первой синхронизации."},
	"plat.events_are":        {"events %s", "события %s"},
	"plat.token_saved":       {"access token saved", "токен доступа сохранён"},
	"plat.no_token":          {"no access token saved", "токен доступа не сохранён"},
	"plat.never":             {"Not synced yet.", "Синхронизации ещё не было."},
	"plat.last_sync":         {"Last sync %s (%s)", "Последняя синхронизация %s (%s)"},
	"plat.result":            {"%d platform users · students linked: %d · logins not on the platform: %d · students without a login: %d", "пользователей платформы: %d · учеников связано: %d · логинов нет на платформе: %d · учеников без логина: %d"},
	"plat.every":             {"automatically every %s", "автоматически каждые %s"},
	"plat.last_error":        {"The sync at %s failed: %s", "Синхронизация в %s не удалась: %s"},
	"plat.settings":          {"Settings", "Настройки"},
	"plat.events":            {"Events whose participants are all loaded (optional; numbers, separated by commas)", "События, все участники которых загружаются (необязательно; номера через запятую)"},
	"plat.token":             {"Access token", "Токен доступа"},
	"plat.token_keep":        {"saved: leave empty to keep it", "сохранён: оставьте пустым, чтобы не менять"},
	"plat.token_hint":        {"An admin of the platform creates it in the platform's Git: Settings → Applications → Generate New Token. It is kept in a file on the server, never shown again and never written to the journal.", "Его создаёт администратор платформы в Git платформы: Settings → Applications → Generate New Token. Токен хранится в файле на сервере, больше не показывается и не пишется в журнал."},
	"plat.saved":             {"Platform settings saved.", "Настройки платформы сохранены."},
	"plat.synced":            {"Synced: %d platform users, %d students linked.", "Синхронизировано: пользователей платформы %d, учеников связано %d."},
	"plat.failed":            {"The sync failed: %s", "Синхронизация не удалась: %s"},
	"plat.busy":              {"A sync is already running. Try again in a minute.", "Синхронизация уже идёт. Попробуйте через минуту."},
	"plat.bad_events":        {"Events are numbers separated by commas (at most 20).", "События — номера через запятую (не больше 20)."},
	"plat.bad_token":         {"That does not look like an access token. Paste it exactly as the platform shows it.", "Это не похоже на токен доступа. Вставьте его точно так, как показывает платформа."},
	"plat.linked":            {"Students who got a login: %d.", "Учеников получили логин: %d."},
	"plat.linked_none":       {"No student could be linked by name unambiguously.", "Ни одного ученика не удалось однозначно связать по имени."},
	"plat.users":             {"Platform users", "Пользователи платформы"},
	"plat.show_all":          {"All", "Все"},
	"plat.show_unlinked":     {"Not linked to a student (%d)", "Не связаны с учеником (%d)"},
	"plat.no_login":          {"Students without a login: %d. Set it on the student page, in the student list file (column \"login\") or with \"Link students by name\".", "Учеников без логина: %d. Его можно задать на странице ученика, в файле со списком учеников (столбец «логин») или кнопкой «Связать учеников по имени»."},
	"plat.col_login":         {"Login", "Логин"},
	"plat.col_name":          {"Name on the platform", "Имя на платформе"},
	"plat.col_xp":            {"XP", "XP"},
	"plat.col_audit":         {"Audit ratio", "Аудит-рейтинг"},
	"plat.col_student":       {"Student", "Ученик"},
	"plat.unlinked":          {"not linked", "не связан"},
	"plat.all_linked":        {"Every platform user is linked to a student.", "Все пользователи платформы связаны с учениками."},
	"plat.no_users":          {"No platform users yet: give students their logins and sync.", "Пользователей платформы пока нет: задайте ученикам логины и синхронизируйте."},
	"plat.on_platform":       {"On the learning platform", "На учебной платформе"},
	"plat.synced_at":         {"as of %s", "по данным на %s"},
	"plat.login_not_found":   {"The login %s was not found on the platform at the last sync (%s). Check the spelling; if the login was added after that sync, press \"Sync now\".", "Логин %s не найден на платформе при последней синхронизации (%s). Проверьте написание; если логин добавлен после неё, нажмите «Синхронизировать сейчас»."},
	"plat.no_student_login":  {"No platform login yet: add it under \"Edit name, group or login\".", "Логин на платформе не задан: добавьте его в разделе «Изменить имя, группу или логин»."},
	"points.title":           {"Social activity points", "Баллы за социальную активность"},
	"points.points":          {"Points", "Баллы"},
	"points.reason_ph":       {"What for? (required)", "За что? (обязательно)"},
	"points.present_hint":    {"The points go to every student checked in right now (the blue tiles above).", "Баллы получат все ученики, которые сейчас отмечены (синие плитки выше)."},
	"points.present_confirm": {"Add these points to every student checked in right now?", "Начислить баллы всем ученикам, которые сейчас отмечены?"},
	"points.add_present":     {"Add to everyone present", "Начислить всем присутствующим"},
	"points.add":             {"Add points", "Начислить"},
	"points.added_present":   {"%d points added to each of %d students.", "Начислено по %d баллов, учеников: %d."},
	"points.added_student":   {"%d points added.", "Начислено баллов: %d."},
	"points.today":           {"Given to everyone present today", "Начислено всем присутствующим сегодня"},
	"points.students":        {"Students", "Учеников"},
	"points.cancel":          {"Cancel", "Отменить"},
	"points.cancel_confirm":  {"Cancel these points? The entry stays in the history, crossed out.", "Отменить эти баллы? Запись останется в истории, зачёркнутой."},
	"points.cancelled":       {"cancelled", "отменено"},
	"points.cancelled_by":    {"cancelled by %s, %s", "отменил(а) %s, %s"},
	"points.cancelled_one":   {"The points are cancelled.", "Баллы отменены."},
	"points.cancelled_batch": {"Cancelled for %d students.", "Отменено у учеников: %d."},
	"points.everyone":        {"everyone present", "всем присутствующим"},
	"points.total":           {"Total: %d", "Всего: %d"},
	"points.none":            {"No points yet.", "Баллов пока нет."},
	"points.bad_points":      {"Points must be a whole number from 1 to %d.", "Баллы — целое число от 1 до %d."},
	"points.no_reason":       {"Write what the points are for.", "Напишите, за что начисляются баллы."},
	"points.nobody":          {"Nobody is checked in right now.", "Сейчас никто не отмечен."},
	"points.no_entry":        {"These points are already cancelled.", "Эти баллы уже отменены."},
	"login.bad":              {"A login has only Latin letters, digits, '.', '_' and '-'.", "Логин — только латинские буквы, цифры, «.», «_» и «-»."},
	"login.taken":            {"The login %s already belongs to %s.", "Логин %s уже принадлежит ученику %s."},
	"ntoday.summary":         {"With a student: %d · logged in without a check-in: %d · free: %d", "С учеником: %d · вход без отметки: %d · свободно: %d"},
	"ntoday.l_busy":          {"student checked in", "ученик отмечен"},
	"ntoday.l_login":         {"logged in, no check-in", "вход без отметки"},
	"ntoday.l_free":          {"not in use", "не используется"},
	"ntoday.no_checkin":      {"no check-in", "без отметки"},
	"ntoday.inactive":        {"inactive", "неактивен"},
	"ntoday.never":           {"no sessions yet", "сеансов не было"},
	"ntoday.rows":            {"Rows", "Рядов"},
	"ntoday.cols":            {"Columns", "Столбцов"},
	"ntoday.reset":           {"Reset layout", "Сбросить раскладку"},
	"ntoday.reset_confirm":   {"Put all computers back in name order, 5 rows of 10?", "Вернуть все компьютеры в порядок по именам, 5 рядов по 10?"},
	"ntoday.drag_hint":       {"Drag a computer onto another one to swap them, or onto an empty cell to move it.", "Перетащите компьютер на другой, чтобы поменять их местами, или на пустую ячейку, чтобы переместить."},
	"ntoday.bad_size":        {"Rows and columns must be numbers from 1 to %d.", "Число рядов и столбцов должно быть от 1 до %d."},
	"ntoday.bad_move":        {"That computer could not be moved. Reload the page and try again.", "Не удалось переместить компьютер. Обновите страницу и попробуйте снова."},
	"ntoday.recent":          {"Recently ended sessions", "Недавно завершённые сессии"},
	"ntoday.recent_hint":     {"Sessions that ended today, most recent first.", "Сессии, завершившиеся сегодня; сначала самые недавние."},
	"ntoday.col_len":         {"Session length", "Длительность сессии"},
	"ntoday.col_ago":         {"Ended", "Завершена"},
	"ntoday.none":            {"No session has ended today yet.", "Сегодня ещё не завершилась ни одна сессия."},
	"ntoday.no_match":        {"No sessions in this group.", "В этой группе нет сессий."},
	"nanom.low":              {"Low-activity sessions", "Сессии с низкой активностью"},
	"nanom.low_hint":         {"Long gaps between active pings, again and again: the typical (median) gap is at least %s. Visits of 30 min or more with agent pings.", "Постоянные большие задержки между активными пингами: типичный (медианный) перерыв не меньше %s. Посещения от 30 минут с данными агента."},
	"nanom.long":             {"Long sessions", "Длинные сессии"},
	"nanom.long_hint":        {"Longer than %s.", "Дольше %s."},
	"nanom.old":              {"Old QR code scans", "Сканы старого QR-кода"},
	"nanom.old_hint":         {"The code was scanned more than %s after it appeared, or after the screen had already shown a newer code. Can mean a photo or a relayed screen.", "Код отсканирован более чем через %s после появления или когда на экране уже был новый код. Возможно, это фото или пересланный экран."},
	"nanom.code_age":         {"Code age", "Возраст кода"},
	"nanom.superseded":       {"newer code on screen", "на экране уже новый код"},
	"nanom.multi":            {"Sessions with 2–3 anomalies", "Сессии с 2–3 аномалиями"},
	"nanom.multi_hint":       {"Visits that are in two or three of the lists above.", "Посещения, попавшие в два или три списка выше."},
	"nanom.gap":              {"Median gap", "Медианный перерыв"},
	"nanom.active":           {"Active pings", "Активных пингов"},
	"nanom.flags":            {"Anomalies", "Аномалии"},
	"nanom.scan":             {"Check-in", "Отметка"},
	"nanom.f_low":            {"low activity", "низкая активность"},
	"nanom.f_long":           {"long", "длинная"},
	"nanom.f_old":            {"old code", "старый код"},
	"dash.col_student":       {"Student", "Студент"},
	"dash.col_status":        {"Status", "Статус"},
	"dash.col_hours":         {"Hours", "Часы"},
	"dash.col_days":          {"Days", "Дней"},
	"dash.col_att":           {"Attendance", "Посещаемость"},
	"dash.col_per_day":       {"H/day", "Ч/день"},
	"dash.col_sessions":      {"Sessions", "Сессий"},
	"dash.col_arrival":       {"Arrival", "Приход"},
	"dash.col_last":          {"Last visit", "Последний визит"},
	"dash.st_active":         {"Active", "Активен"},
	"dash.st_left":           {"Left", "Выбыл"},
	"dash.empty":             {"No attendance in this period.", "За этот период посещений нет."},
	"dash.cell":              {"%s, %s: %s h", "%s, %s: %s ч"},
	"dash.bar":               {"%s: %s", "%s: %s"},

	"wd.0": {"Sun", "Вс"},
	"wd.1": {"Mon", "Пн"},
	"wd.2": {"Tue", "Вт"},
	"wd.3": {"Wed", "Ср"},
	"wd.4": {"Thu", "Чт"},
	"wd.5": {"Fri", "Пт"},
	"wd.6": {"Sat", "Сб"},

	// Admin: login
	"login.title":    {"Staff login", "Вход для сотрудников"},
	"login.username": {"Username", "Логин"},
	"login.password": {"Password", "Пароль"},
	"login.submit":   {"Log in", "Войти"},
	"login.failed":   {"Wrong username or password.", "Неверный логин или пароль."},
	"login.locked":   {"Too many failed attempts. Try again in a few minutes.", "Слишком много неудачных попыток. Попробуйте через несколько минут."},

	// Admin: Today
	"today.title":          {"Today", "Сегодня"},
	"today.status":         {"System status", "Состояние системы"},
	"today.last_heartbeat": {"Last heartbeat", "Последний сигнал от ПК"},
	"today.last_scan":      {"Last check-in", "Последняя отметка"},
	"today.bot_poll":       {"Bot last reached Telegram", "Бот последний раз связывался с Telegram"},
	"today.bot_reply":      {"Bot's last reply", "Последний ответ бота"},
	"today.bot_off":        {"bot disabled", "бот выключен"},
	"today.bot_error":      {"Bot error: %s", "Ошибка бота: %s"},
	"today.open_visits":    {"Open visits", "Открытых посещений"},
	"today.pcs":            {"PCs", "Компьютеры"},
	"today.session_since":  {"Logged in since", "Вход с"},
	"today.heartbeat":      {"Last heartbeat", "Последний сигнал"},
	"today.idle":           {"Idle", "Бездействие"},
	"today.checked_in":     {"Checked in", "Отмечены"},
	"today.no_checkin":     {"session, no check-in", "сеанс без отметки"},
	"today.no_session":     {"no session", "нет сеанса"},
	"today.last_seen":      {"no session · last heartbeat %s", "нет сеанса · последний сигнал %s"},
	"today.idle_unknown":   {"unknown", "неизвестно"},
	"today.pcs_summary":    {"%d PCs with a session, %d of them without a check-in.", "Сеансов на ПК: %d, из них без отметки: %d."},
	"today.today_visits":   {"Visits today", "Посещения за сегодня"},

	// Admin: Hours
	"hours.title":        {"Hours", "Часы"},
	"hours.day":          {"Day", "День"},
	"hours.week":         {"Week", "Неделя"},
	"hours.month":        {"Month", "Месяц"},
	"hours.total":        {"Total", "Всего"},
	"hours.visits":       {"Visits", "Посещений"},
	"hours.first":        {"First arrival", "Первый приход"},
	"hours.last":         {"Last departure", "Последний уход"},
	"hours.csv":          {"Download CSV", "Скачать CSV"},
	"hours.policy":       {"Rules: %s", "Правила: %s"},
	"hours.policy_exact": {"exact minutes, from scan to last activity", "точные минуты, от скана до последней активности"},
	"hours.rounding":     {"daily totals rounded to %s", "дневные итоги округляются до %s"},
	"hours.min_visit":    {"visits under %s ignored", "посещения короче %s не учитываются"},
	"hours.cap":          {"at most %s per day", "не более %s в день"},
	"hours.no_visits":    {"Show students without visits", "Показывать учеников без посещений"},
	"hours.in_progress":  {"still here", "ещё здесь"},

	// Admin: Students
	"students.title":          {"Students", "Ученики"},
	"students.import":         {"Import or update from CSV", "Импорт или обновление из CSV"},
	"students.import_hint":    {"Columns: name, group, and optionally id and login (the platform login). Comma or semicolon separated. Existing students are matched by id, then by login, otherwise by exact name.", "Столбцы: name/имя, group/группа и необязательные id и login/логин (логин на платформе). Разделитель — запятая или точка с запятой. Существующие ученики ищутся по id, затем по логину, иначе по точному имени."},
	"students.import_done":    {"Imported: %d added, %d updated, %d unchanged.", "Импорт: добавлено %d, обновлено %d, без изменений %d."},
	"students.add":            {"Add a student", "Добавить ученика"},
	"students.status":         {"Telegram", "Telegram"},
	"students.linked":         {"linked", "привязан"},
	"students.code_pending":   {"code issued, not used", "ссылка выдана, не использована"},
	"students.code_expired":   {"code expired", "ссылка истекла"},
	"students.not_linked":     {"not linked", "не привязан"},
	"students.archived":       {"archived", "в архиве"},
	"students.gen_code":       {"New link", "Новая ссылка"},
	"students.gen_all":        {"Create links for everyone not yet linked", "Создать ссылки для всех непривязанных"},
	"students.gen_group":      {"Create links for everyone in %s not yet linked", "Создать ссылки для всех непривязанных в группе %s"},
	"students.unlink":         {"Unlink", "Отвязать"},
	"students.unlink_confirm": {"Unlink this Telegram account? The student will need a new link.", "Отвязать этот аккаунт Telegram? Ученику понадобится новая ссылка."},
	"students.archive":        {"Archive", "В архив"},
	"students.unarchive":      {"Restore", "Вернуть"},
	"students.show_archived":  {"Show archived", "Показать архив"},
	"students.slips":          {"Print link slips", "Печать ссылок"},
	"students.slips_hint":     {"Hand each slip to its student privately. Whoever opens a link first gets that identity.", "Выдавайте каждую ссылку лично ученику. Кто первым откроет ссылку, тот и будет привязан."},
	"students.link":           {"Personal link", "Личная ссылка"},
	"students.expires":        {"Valid until %s", "Действует до %s"},
	"students.summary":        {"%d students: %d linked, %d with an unused link, %d not linked.", "Учеников: %d, привязано: %d, с неиспользованной ссылкой: %d, без привязки: %d."},
	"students.tg_account":     {"Telegram account", "Аккаунт Telegram"},
	"students.edit":           {"Edit name, group or login", "Изменить имя, группу или логин"},
	"students.login":          {"Platform login", "Логин на платформе"},
	"students.login_optional": {"Platform login (optional)", "Логин на платформе (необязательно)"},

	// Admin: Student detail
	"student.visits":         {"Visits", "Посещения"},
	"student.add_visit":      {"Add a missed visit", "Добавить пропущенное посещение"},
	"student.manual":         {"added by staff", "добавлено вручную"},
	"student.edited":         {"edited", "изменено"},
	"student.deleted":        {"deleted", "удалено"},
	"student.was":            {"was %s – %s on %s", "было %s – %s на %s"},
	"student.corrections":    {"Corrections", "Исправления"},
	"student.latency":        {"scan took %s", "скан через %s"},
	"student.stale":          {"old code", "старый код"},
	"student.month_total":    {"Total for the month: %s", "Итого за месяц: %s"},
	"student.delete_confirm": {"Delete this visit? Enter a reason below.", "Удалить это посещение? Укажите причину."},

	// Admin: Anomalies
	"anom.title":         {"Anomalies", "Аномалии"},
	"anom.latency":       {"Slow or old-code scans", "Медленные сканы или сканы старого кода"},
	"anom.latency_hint":  {"The code was scanned more than %s after it appeared, or after the screen had already shown a newer code. Can mean a photo or a relayed screen.", "Код отсканирован более чем через %s после появления или после того, как на экране уже был новый код. Может означать фото или трансляцию экрана."},
	"anom.multi_pc":      {"One student on several PCs", "Один ученик на нескольких ПК"},
	"anom.multi_pc_hint": {"Checked in on a second PC within %s while the first PC was still in use. Can mean a lent Telegram account.", "Отметка на втором ПК в пределах %s, пока первый ещё использовался. Может означать, что аккаунт Telegram передали другому."},
	"anom.many":          {"Several students on one PC session", "Несколько учеников в одном сеансе ПК"},
	"anom.many_hint":     {"Each new check-in ended the previous student's visit. Normal when students swap without logging out, but worth a look.", "Каждая новая отметка закрывала посещение предыдущего ученика. Обычно это смена ученика без выхода из системы, но стоит проверить."},
	"anom.auto":          {"Auto-closed visits", "Автоматически закрытые посещения"},
	"anom.auto_hint":     {"Closed by the nightly cleanup. Check that the end time is right.", "Закрыты ночной очисткой. Проверьте время окончания."},
	"anom.failed":        {"Failed scans", "Неудачные сканы"},
	"anom.result":        {"Result", "Результат"},
	"anom.students":      {"Students", "Ученики"},
	"anom.first_pc":      {"First PC", "Первый ПК"},
	"anom.second_pc":     {"Second PC", "Второй ПК"},
	"anom.tg_user":       {"Telegram user", "Пользователь Telegram"},

	// Scan results (failed scan log)
	"scan.bad_token":    {"invalid code", "неверный код"},
	"scan.expired":      {"expired code", "код устарел"},
	"scan.used":         {"code already used", "код уже использован"},
	"scan.unknown_host": {"unknown PC", "неизвестный ПК"},
	"scan.not_linked":   {"account not linked", "аккаунт не привязан"},
	"scan.no_session":   {"no session on the PC", "нет сеанса на ПК"},

	// Admin: Audit
	"audit.title": {"Audit log", "Журнал действий"},
}
