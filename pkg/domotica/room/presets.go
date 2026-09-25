package room

// Presets son los botones de la página, agrupados por la capa que se espera que
// los decida. Los «direct» salen de las plantillas de la demo (un test lo
// comprueba contra la capa 1); los demás son justo lo que las capas rápidas no
// saben y escalan.
var Presets = []Preset{
	{"es", "enciende la luz del salón", "direct"},
	{"es", "apaga las luces de la cocina", "direct"},
	{"es", "pon la luz del dormitorio en azul", "direct"},
	{"es", "pon la temperatura a veintidós grados", "direct"},
	{"es", "cierra las persianas del dormitorio", "direct"},
	{"es", "enciende la tele", "direct"},
	{"es", "sube el volumen de la tele", "direct"},
	{"es", "ajusta la velocidad del ventilador al sesenta por ciento", "direct"},
	{"es", "activa la alarma", "direct"},
	{"es", "baja un poco las persianas del salón", "paraphrase"},
	{"es", "¿puedes poner el salón en azul?", "paraphrase"},
	{"es", "calefacción a veintiuno", "paraphrase"},
	{"es", "aquí hace frío", "indirect"},
	{"es", "no veo nada", "indirect"},
	{"es", "me voy de casa", "indirect"},
	{"es", "pon la persiana a la mitad", "indirect"},
	{"es", "apaga todo y cierra la puerta", "indirect"},
	{"es", "enciende la luz y baja la persiana", "indirect"},
	{"es", "pon una alarma a las siete", "oos"},
	{"es", "¿qué tiempo hará mañana?", "oos"},

	{"en", "turn on the kitchen lights", "direct"},
	{"en", "turn off the lights in the living room", "direct"},
	{"en", "set the lights in the bedroom to red", "direct"},
	{"en", "set the thermostat to 22 degrees", "direct"},
	{"en", "open the blinds", "direct"},
	{"en", "turn on the tv", "direct"},
	{"en", "turn down the volume", "direct"},
	{"en", "lock the door", "direct"},
	{"en", "set the fan to 40 percent", "direct"},
	{"en", "kill the lights", "paraphrase"},
	{"en", "crank up the volume", "paraphrase"},
	{"en", "heat to 22 please", "paraphrase"},
	{"en", "it's freezing in here", "indirect"},
	{"en", "i can't see anything", "indirect"},
	{"en", "i'm heading out for the day", "indirect"},
	{"en", "put the blinds halfway", "indirect"},
	{"en", "turn everything off and lock the door", "indirect"},
	{"en", "open the blinds and turn on the fan", "indirect"},
	{"en", "set an alarm for 7 am", "oos"},
	{"en", "order a pizza", "oos"},
}
