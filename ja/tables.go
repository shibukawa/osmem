package ja

import "strings"

// defaultStopTags mirrors Lucene's stoptags.txt (the active entries).
var defaultStopTags = []string{
	"接続詞", "助詞", "助詞-格助詞", "助詞-格助詞-一般", "助詞-格助詞-引用", "助詞-格助詞-連語", "助詞-接続助詞",
	"助詞-係助詞", "助詞-副助詞", "助詞-間投助詞", "助詞-並立助詞", "助詞-終助詞", "助詞-副助詞／並立助詞／終助詞",
	"助詞-連体化", "助詞-副詞化", "助詞-特殊", "助動詞", "記号", "記号-一般", "記号-読点", "記号-句点", "記号-空白",
	"記号-括弧開", "記号-括弧閉", "その他-間投", "フィラー", "非言語音", "語断片",
}

// defaultStopWords mirrors Lucene's Japanese stopwords.txt.
var defaultStopWords = []string{
	"の", "に", "は", "を", "た", "が", "で", "て", "と", "し", "れ", "さ", "ある", "いる", "も", "する", "から", "な", "こと",
	"として", "い", "や", "れる", "など", "なっ", "ない", "この", "ため", "その", "あっ", "よう", "また", "もの", "という",
	"あり", "まで", "られ", "なる", "へ", "か", "だ", "これ", "によって", "により", "おり", "より", "による", "ず", "なり",
	"られる", "において", "ば", "なかっ", "なく", "しかし", "について", "せ", "だっ", "その後", "できる", "それ", "う",
	"ので", "なお", "のみ", "でき", "き", "つ", "における", "および", "いう", "さらに", "でも", "ら", "たり", "その他",
	"に関する", "たち", "ます", "ん", "なら", "に対して", "特に", "せる", "及び", "これら", "とき", "では", "にて", "ほか",
	"ながら", "うち", "そして", "とともに", "ただし", "かつて", "それぞれ", "または", "お", "ほど", "ものの", "に対する",
	"ほとんど", "と共に", "といった", "です", "とも", "ところ", "ここ",
}

// romaji is a Hepburn table for katakana readings (kuromoji_readingform
// with use_romaji).
var romaji = map[string]string{
	"ア": "a", "イ": "i", "ウ": "u", "エ": "e", "オ": "o",
	"カ": "ka", "キ": "ki", "ク": "ku", "ケ": "ke", "コ": "ko", "ガ": "ga", "ギ": "gi", "グ": "gu", "ゲ": "ge", "ゴ": "go",
	"サ": "sa", "シ": "shi", "ス": "su", "セ": "se", "ソ": "so", "ザ": "za", "ジ": "ji", "ズ": "zu", "ゼ": "ze", "ゾ": "zo",
	"タ": "ta", "チ": "chi", "ツ": "tsu", "テ": "te", "ト": "to", "ダ": "da", "ヂ": "ji", "ヅ": "zu", "デ": "de", "ド": "do",
	"ナ": "na", "ニ": "ni", "ヌ": "nu", "ネ": "ne", "ノ": "no",
	"ハ": "ha", "ヒ": "hi", "フ": "fu", "ヘ": "he", "ホ": "ho", "バ": "ba", "ビ": "bi", "ブ": "bu", "ベ": "be", "ボ": "bo",
	"パ": "pa", "ピ": "pi", "プ": "pu", "ペ": "pe", "ポ": "po",
	"マ": "ma", "ミ": "mi", "ム": "mu", "メ": "me", "モ": "mo", "ヤ": "ya", "ユ": "yu", "ヨ": "yo",
	"ラ": "ra", "リ": "ri", "ル": "ru", "レ": "re", "ロ": "ro", "ワ": "wa", "ヲ": "o", "ン": "n", "ヴ": "vu",
	"キャ": "kya", "キュ": "kyu", "キョ": "kyo", "ギャ": "gya", "ギュ": "gyu", "ギョ": "gyo",
	"シャ": "sha", "シュ": "shu", "ショ": "sho", "ジャ": "ja", "ジュ": "ju", "ジョ": "jo",
	"チャ": "cha", "チュ": "chu", "チョ": "cho", "ニャ": "nya", "ニュ": "nyu", "ニョ": "nyo",
	"ヒャ": "hya", "ヒュ": "hyu", "ヒョ": "hyo", "ビャ": "bya", "ビュ": "byu", "ビョ": "byo", "ピャ": "pya", "ピュ": "pyu", "ピョ": "pyo",
	"ミャ": "mya", "ミュ": "myu", "ミョ": "myo", "リャ": "rya", "リュ": "ryu", "リョ": "ryo",
	"ファ": "fa", "フィ": "fi", "フェ": "fe", "フォ": "fo", "ティ": "ti", "ディ": "di", "デュ": "dyu", "ウィ": "wi", "ウェ": "we", "ウォ": "wo",
	"ヴァ": "va", "ヴィ": "vi", "ヴェ": "ve", "ヴォ": "vo", "シェ": "she", "ジェ": "je", "チェ": "che", "トゥ": "tu", "ドゥ": "du",
}

// toRomaji converts a katakana reading to Hepburn romaji. Characters
// without a mapping are kept.
func toRomaji(s string) string {
	rs := []rune(s)
	var sb strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] == 'ッ' && i+1 < len(rs) {
			next := romaji[string(rs[i+1])]
			if next == "" && i+2 < len(rs) {
				next = romaji[string(rs[i+1:i+3])]
			}
			if next != "" {
				sb.WriteString(next[:1])
			}
			continue
		}
		if rs[i] == 'ー' {
			continue
		}
		if i+1 < len(rs) {
			if r, ok := romaji[string(rs[i:i+2])]; ok {
				sb.WriteString(r)
				i++
				continue
			}
		}
		if r, ok := romaji[string(rs[i])]; ok {
			sb.WriteString(r)
			continue
		}
		sb.WriteRune(rs[i])
	}
	return sb.String()
}
