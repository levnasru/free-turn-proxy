package udprelay

import "testing"

// drive прокручивает ticks тиков с постоянным gradient, применяя каждое
// решение к k ровно так, как это делает refreshLoop в бою. Возвращает
// итоговый k и номера тиков (с 1), на которых было реальное действие -
// проверять именно НОМЕРА тиков, а не только их количество: половина смысла
// слоя в том, КОГДА он имеет право сработать. Второй возврат decide (refused)
// здесь не нужен - он про то, что в лог, а не про то, что с K; его отдельно
// проверяет TestAutoscaleRefusalIsReportedAtBothBounds.
func drive(d *autoscaleDecider, gradient float64, k int, b autoscaleBounds, ticks int) (int, []int) {
	var at []int
	for i := 1; i <= ticks; i++ {
		if delta, _ := d.decide(gradient, k, b); delta != 0 {
			k += delta
			at = append(at, i)
		}
	}
	return k, at
}

func TestAutoscaleZone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		gradient float64
		want     int
	}{
		{"ровно на пороге роста", 1.00, 1},
		{"выше порога роста (avgRTT == дно)", 1.20, 1},
		{"ровно на пороге сжатия", 0.70, -1},
		{"глубоко в деградации", 0.05, -1},
		{"середина нейтральной зоны", 0.85, 0},
		{"чуть ниже порога роста", 0.999, 0},
		{"чуть выше порога сжатия", 0.701, 0},
		{"нет данных - нейтраль, не здоровье", 0, 0},
		{"отрицательный мусор - нейтраль", -1, 0},
	}
	for _, c := range cases {
		if got := autoscaleZone(c.gradient); got != c.want {
			t.Errorf("%s: autoscaleZone(%.3f) = %d, ожидалось %d", c.name, c.gradient, got, c.want)
		}
	}
}

func TestAutoscaleBoundsFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		baseK    int
		min, max int
	}{
		{30, 15, 30}, // семейный профиль
		{20, 10, 20}, // профиль Ростика
		{3, 1, 3},
		{1, 1, 1},
		{0, 1, 1}, // мусор на входе не должен давать пол 0 (hot-set без слотов)
	}
	for _, c := range cases {
		got := autoscaleBoundsFor(c.baseK)
		if got.min != c.min || got.max != c.max {
			t.Errorf("autoscaleBoundsFor(%d) = [%d..%d], ожидалось [%d..%d]",
				c.baseK, got.min, got.max, c.min, c.max)
		}
	}
}

func TestAutoscaleGrowWaitsForRunAndCooldown(t *testing.T) {
	t.Parallel()
	// Сигнал "канал у дна" держится непрерывно. Первый рост не раньше, чем
	// истечёт кулдаун (10 тиков = 5 мин), даже при том, что серия (6 тиков)
	// набралась раньше - гейта два, и они независимы.
	d := &autoscaleDecider{}
	k, at := drive(d, 1.20, 20, autoscaleBoundsFor(30), 30)
	want := []int{10, 20, 30}
	if len(at) != len(want) {
		t.Fatalf("ожидалось %d роста на тиках %v, получено %d на %v", len(want), want, len(at), at)
	}
	for i := range want {
		if at[i] != want[i] {
			t.Fatalf("рост на тиках %v, ожидалось %v", at, want)
		}
	}
	if k != 23 {
		t.Fatalf("K = %d, ожидалось 23 (20 + 3 шага)", k)
	}
}

func TestAutoscaleShrinkIsFasterThanGrow(t *testing.T) {
	t.Parallel()
	// Та же непрерывность сигнала, но в сторону деградации: серия 4 тика,
	// кулдаун 6 - первое сжатие на 6-м тике против 10-го у роста, дальше
	// каждые 6 тиков. Асимметрия сознательная, см. комментарии в autoscale.go.
	d := &autoscaleDecider{}
	k, at := drive(d, 0.30, 20, autoscaleBoundsFor(30), 30)
	if len(at) != 5 || at[0] != 6 || at[4] != 30 {
		t.Fatalf("сжатия на тиках %v, ожидалось 5 штук начиная с 6-го каждые 6 тиков", at)
	}
	if k != 15 {
		t.Fatalf("K = %d, ожидалось 15 (20 - 5 шагов)", k)
	}
}

func TestAutoscaleNeutralTickWipesTheRun(t *testing.T) {
	t.Parallel()
	// 9 тиков уверенного роста (действия ещё нет - кулдаун 10), затем ОДИН
	// тик в нейтральной зоне. Он должен стереть накопленную серию целиком,
	// а не просто пропустить ход: иначе слой действует по сигналу, который
	// уже прерывался.
	d := &autoscaleDecider{}
	b := autoscaleBoundsFor(30)
	k := 20
	var at []int
	for i := 1; i <= 16; i++ {
		g := 1.20
		if i == 10 {
			g = 0.85 // нейтраль ровно на том тике, где иначе был бы рост
		}
		if delta, _ := d.decide(g, k, b); delta != 0 {
			k += delta
			at = append(at, i)
		}
	}
	if len(at) != 1 || at[0] != 16 {
		t.Fatalf("рост на тиках %v, ожидался ровно один на 16-м (10-й съеден нейтралью, серия набирается заново)", at)
	}
}

func TestAutoscaleCeilingHoldsAndDoesNotBurnCooldown(t *testing.T) {
	t.Parallel()
	// K уже на потолке (= стартовое N): сколько бы сигнал ни просил роста,
	// действия нет - выше N лезть в квоту TURN нельзя.
	d := &autoscaleDecider{}
	b := autoscaleBoundsFor(30)
	k, at := drive(d, 1.20, 30, b, 30)
	if k != 30 || len(at) != 0 {
		t.Fatalf("K = %d, действия на %v; ожидалось стоять на 30 без действий", k, at)
	}

	// И сразу за этим - деградация. Кулдаун упёршимися попытками не сожжён,
	// значит сжатие приходит по своей серии (4 тика), а не ждёт лишнего.
	_, at = drive(d, 0.30, 30, b, 4)
	if len(at) != 1 || at[0] != 4 {
		t.Fatalf("сжатия на %v, ожидалось одно на 4-м тике - попытки в потолок не должны жечь кулдаун", at)
	}
}

func TestAutoscaleFloorHolds(t *testing.T) {
	t.Parallel()
	// Пол = N/2: даже при непрерывной деградации больше половины полосы
	// ставка "меньше путей = меньше очереди" отдать не может.
	d := &autoscaleDecider{}
	k, at := drive(d, 0.05, 15, autoscaleBoundsFor(30), 30)
	if k != 15 || len(at) != 0 {
		t.Fatalf("K = %d, действия на %v; ожидалось стоять на полу 15 без действий", k, at)
	}
}

func TestAutoscalePullsManualExcursionBackToCeiling(t *testing.T) {
	t.Parallel()
	// K выше потолка бывает только после ручного stdin-"grow". Зажим тянет
	// его назад к N по одному шагу за раз, даже когда сигнал просит роста.
	d := &autoscaleDecider{}
	k, at := drive(d, 1.20, 35, autoscaleBoundsFor(30), 30)
	if k != 32 || len(at) != 3 {
		t.Fatalf("K = %d за %d действий, ожидалось 32 за 3 шага вниз к потолку 30", k, len(at))
	}
}

func TestAutoscaleRefusalIsReportedAtBothBounds(t *testing.T) {
	t.Parallel()
	// Отказ по границе обязан отличаться от "сигнала не было": оба дают
	// delta = 0, но у отказа refused = зона, которую съел зажим. Без этого
	// различия у того, кто крутится на своём N, лог молчит ВСЕГДА - ровно это
	// и вышло в живом прогоне 2026-08-30, где серия дважды дошла до попытки и
	// не оставила в логе ни строки.
	b := autoscaleBoundsFor(30)

	// Потолок. До истечения кулдауна попытки нет - значит и отказывать нечему:
	// молчание этих тиков само по себе часть контракта.
	d := &autoscaleDecider{}
	for i := 1; i < autoscaleGrowCooldownTicks; i++ {
		if delta, refused := d.decide(1.20, 30, b); delta != 0 || refused != 0 {
			t.Fatalf("тик %d: delta=%d refused=%d, до истечения кулдауна ожидались нули", i, delta, refused)
		}
	}
	if delta, refused := d.decide(1.20, 30, b); delta != 0 || refused != 1 {
		t.Fatalf("на потолке: delta=%d refused=%d, ожидалось 0 и +1", delta, refused)
	}

	// Пол. Своя серия и свой кулдаун, поэтому попытка приходит раньше.
	d = &autoscaleDecider{}
	for i := 1; i < autoscaleShrinkCooldownTicks; i++ {
		if delta, refused := d.decide(0.30, 15, b); delta != 0 || refused != 0 {
			t.Fatalf("тик %d: delta=%d refused=%d, до истечения кулдауна ожидались нули", i, delta, refused)
		}
	}
	if delta, refused := d.decide(0.30, 15, b); delta != 0 || refused != -1 {
		t.Fatalf("на полу: delta=%d refused=%d, ожидалось 0 и -1", delta, refused)
	}

	// А реальное действие отказом НЕ помечается - иначе лог утверждал бы
	// "уперлись в границу" на каждом успешном ресайзе.
	d = &autoscaleDecider{}
	var applied int
	for i := 1; i <= autoscaleGrowCooldownTicks; i++ {
		delta, refused := d.decide(1.20, 20, b)
		if refused != 0 {
			t.Fatalf("тик %d: refused=%d при K=20 внутри [%d..%d]", i, refused, b.min, b.max)
		}
		applied += delta
	}
	if applied != 1 {
		t.Fatalf("суммарная дельта %+d, ожидался ровно один рост", applied)
	}
}
