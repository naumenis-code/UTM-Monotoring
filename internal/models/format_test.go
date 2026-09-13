package models

import "testing"

func TestPrettyOrgName(t *testing.T) {
	cases := map[string]string{
		"ИП СМИРНОВ ОЛЕГ НИКОЛАЕВИЧ":                           "ИП Смирнов О.Н.",
		"ип иванов пётр сергеевич":                             "ИП Иванов П.С.",
		"ИНДИВИДУАЛЬНЫЙ ПРЕДПРИНИМАТЕЛЬ ПЕТРОВ ИВАН ИВАНОВИЧ":  "ИП Петров И.И.",
		"ИП ИВАНОВ-ПЕТРОВ СЕРГЕЙ ИВАНОВИЧ":                     "ИП Иванов-Петров С.И.",
		`ООО "Ромашка"`:                                        `ООО "Ромашка"`,
		"Общество с ограниченной ответственностью \"Ромашка\"": "Общество с ограниченной ответственностью \"Ромашка\"",
		"":               "",
		"ИП Иванов":      "ИП Иванов", // unexpected shape (not exactly 3 words) left as-is
		"ИП Иванов Иван": "ИП Иванов Иван",
	}
	for input, want := range cases {
		if got := PrettyOrgName(input); got != want {
			t.Errorf("PrettyOrgName(%q) = %q, want %q", input, got, want)
		}
	}
}
