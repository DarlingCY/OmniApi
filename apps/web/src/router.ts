import { createRouter, createWebHistory } from 'vue-router';
import App from './App.vue';

export const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', name: 'overview', component: App },
    { path: '/providers', name: 'providers', component: App },
    { path: '/monitor', name: 'monitor', component: App },
    { path: '/runtime-logs', name: 'runtimeLogs', component: App },
    { path: '/access-keys', name: 'accessKeys', component: App },
    { path: '/settings', name: 'settings', component: App },
    { path: '/:pathMatch(.*)*', redirect: '/overview' },
  ],
});
