import { createApp, h } from 'vue';
import { RouterView } from 'vue-router';
import {
	Alert,
	Button,
	Checkbox,
	DatePicker,
	Drawer,
	Empty,
	Form,
	Input,
	Message,
	Modal,
	Pagination,
	Select,
	Spin,
	Switch,
	Tag,
	Tooltip,
} from '@arco-design/web-vue';
import {
	IconDashboard,
	IconCopy,
	IconCode,
	IconDelete,
	IconDown,
	IconEdit,
	IconEye,
	IconEyeInvisible,
	IconLock,
	IconPlus,
	IconRefresh,
	IconSafe,
	IconStorage,
	IconSun,
	IconMoon,
	IconUp,
} from '@arco-design/web-vue/es/icon';
import '@arco-design/web-vue/es/alert/style/css.js';
import '@arco-design/web-vue/es/button/style/css.js';
import '@arco-design/web-vue/es/checkbox/style/css.js';
import '@arco-design/web-vue/es/date-picker/style/css.js';
import '@arco-design/web-vue/es/drawer/style/css.js';
import '@arco-design/web-vue/es/empty/style/css.js';
import '@arco-design/web-vue/es/form/style/css.js';
import '@arco-design/web-vue/es/input/style/css.js';
import '@arco-design/web-vue/es/message/style/css.js';
import '@arco-design/web-vue/es/modal/style/css.js';
import '@arco-design/web-vue/es/pagination/style/css.js';
import '@arco-design/web-vue/es/select/style/css.js';
import '@arco-design/web-vue/es/spin/style/css.js';
import '@arco-design/web-vue/es/switch/style/css.js';
import '@arco-design/web-vue/es/tag/style/css.js';
import '@arco-design/web-vue/es/tooltip/style/css.js';
import './styles.css';
import { router } from './router';

const app = createApp({ render: () => h(RouterView) }).use(router);
for (const component of [Alert, Button, Checkbox, DatePicker, Drawer, Empty, Form, Input, Message, Modal, Pagination, Select, Spin, Switch, Tag, Tooltip]) {
	app.use(component);
}
for (const [name, component] of Object.entries({
	IconDashboard,
	IconCopy,
	IconCode,
	IconDelete,
	IconDown,
	IconEdit,
	IconEye,
	IconEyeInvisible,
	IconLock,
	IconPlus,
	IconRefresh,
	IconSafe,
	IconStorage,
	IconSun,
	IconMoon,
	IconUp,
})) app.component(name, component);
void router.isReady().then(() => app.mount('#app'));
