package tasks

import (
	"regexp"
	"strings"
)

// cssColorNames is the extended color keyword set from CSS Color Module
// Level 3, §4.3, which the TaskList "color" property is restricted to.
var cssColorNames = map[string]bool{}

func init() {
	for _, name := range strings.Fields(`
		aliceblue antiquewhite aqua aquamarine azure beige bisque black
		blanchedalmond blue blueviolet brown burlywood cadetblue chartreuse
		chocolate coral cornflowerblue cornsilk crimson cyan darkblue darkcyan
		darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta
		darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen
		darkslateblue darkslategray darkslategrey darkturquoise darkviolet
		deeppink deepskyblue dimgray dimgrey dodgerblue firebrick floralwhite
		forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green
		greenyellow grey honeydew hotpink indianred indigo ivory khaki
		lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral
		lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey
		lightpink lightsalmon lightseagreen lightskyblue lightslategray
		lightslategrey lightsteelblue lightyellow lime limegreen linen
		magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple
		mediumseagreen mediumslateblue mediumspringgreen mediumturquoise
		mediumvioletred midnightblue mintcream mistyrose moccasin
		navajowhite navy oldlace olive olivedrab orange orangered orchid
		palegoldenrod palegreen paleturquoise palevioletred papayawhip
		peachpuff peru pink plum powderblue purple red rosybrown royalblue
		saddlebrown salmon sandybrown seagreen seashell sienna silver
		skyblue slateblue slategray slategrey snow springgreen steelblue tan
		teal thistle tomato turquoise violet wheat white whitesmoke yellow
		yellowgreen
	`) {
		cssColorNames[name] = true
	}
}

var hexColorPattern = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// validColor checks a TaskList color: a CSS3 extended color keyword
// (case-insensitively) or an RGB hex value.
func validColor(s string) bool {
	return cssColorNames[strings.ToLower(s)] || hexColorPattern.MatchString(s)
}
