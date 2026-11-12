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
	"bot.status_open":   {"Checked in at %s since %s. Today: %s.", "Вы отмечены на %s с %s. Сегодня: %s."},
	"bot.status_none":   {"You're not checked in right now. Today: %s.", "Сейчас вы не отмечены. Сегодня: %s."},
	"bot.closed":        {"Checked out at %s (%s, %s). Today: %s.", "Отметка закрыта в %s (%s, %s). Сегодня: %s."},
	"bot.rescan_hint":   {"If you're still at the PC, scan the QR code again.", "Если вы всё ещё за компьютером, отсканируйте QR-код ещё раз."},
	"bot.help":          {"Scan the QR code on a school PC screen to check in.\n\n/status — current visit and today's hours\n/checkout — check out now\n/id — your Telegram ID", "Чтобы отметиться, отсканируйте QR-код на экране школьного компьютера.\n\n/status — текущее посещение и часы за сегодня\n/checkout — отметить уход сейчас\n/id — ваш Telegram ID"},
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
	"dash.col_student":      {"Student", "Студент"},
	"dash.col_status":       {"Status", "Статус"},
	"dash.col_hours":        {"Hours", "Часы"},
	"dash.col_days":         {"Days", "Дней"},
	"dash.col_att":          {"Attendance", "Посещаемость"},
	"dash.col_per_day":      {"H/day", "Ч/день"},
	"dash.col_sessions":     {"Sessions", "Сессий"},
	"dash.col_arrival":      {"Arrival", "Приход"},
	"dash.col_last":         {"Last visit", "Последний визит"},
	"dash.st_active":        {"Active", "Активен"},
	"dash.st_left":          {"Left", "Выбыл"},
	"dash.empty":            {"No attendance in this period.", "За этот период посещений нет."},
	"dash.cell":             {"%s, %s: %s h", "%s, %s: %s ч"},
	"dash.bar":              {"%s: %s", "%s: %s"},

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
	"students.import_hint":    {"Columns: name, group, id (optional). Comma or semicolon separated. Existing students are matched by id, otherwise by exact name.", "Столбцы: name/имя, group/группа, id (необязательно). Разделитель — запятая или точка с запятой. Существующие ученики ищутся по id, иначе по точному имени."},
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
	"students.edit":           {"Edit name or group", "Изменить имя или группу"},

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
