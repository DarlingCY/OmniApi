import { createApp, h } from 'vue';
import { RouterView } from 'vue-router';
import ArcoVue from '@arco-design/web-vue';
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
import '@arco-design/web-vue/dist/arco.css';
import './styles.css';
import { router } from './router';

const app = createApp({ render: () => h(RouterView) }).use(ArcoVue).use(router);
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
